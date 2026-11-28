package store

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/godwit-email/godwit/internal/perftrace"
)

// Keeping the WAL small beside the readers (ADR 0052).
//
// A checkpoint copies WAL frames back into the database up to the oldest
// snapshot a reader holds, and the WAL restarts from its start only after
// a checkpoint copied every frame. On perf/store-readers2, readers ran
// statements of seconds back to back, some reader always held an old
// snapshot, and the WAL grew to 1.2 GB in 27 s. Here every connection is
// in this process, so the store coordinates:
//
//   - W keeps wal_autocheckpoint = 1000 and gets journal_size_limit =
//     64 MiB, which cuts the file back after a restart.
//   - After commits (at most once per checkpointEvery), a PASSIVE
//     checkpoint on W. It never blocks anyone.
//   - When it could not copy every frame and the WAL holds more than
//     walHoldAt, B is held for one statement (the coordinator takes it at
//     the front of B's queue) and PASSIVE is tried again a few times while
//     the interactive readers' short statements end. Writes go on.
//   - Above walRestartAt (a safety net that should not fire), W is held
//     as well and RESTART is tried for up to restartFor.
//   - After idleAfter with no statement, and at Close, TRUNCATE empties the
//     file.

const (
	checkpointEvery = time.Second
	walHoldAt       = 16 << 20
	walRestartAt    = 256 << 20
	holdTries       = 5
	holdPause       = 20 * time.Millisecond
	holdWait        = 2 * time.Second
	restartFor      = 2 * time.Second
	idleAfter       = 30 * time.Second
)

// checkpointer is the coordinator's goroutine and its settings (tests
// shorten them).
type checkpointer struct {
	every, idle time.Duration
	holdAt      int64
	restartAt   int64
	pageSize    int64

	stop chan struct{}
	done chan struct{}
	once sync.Once
}

// ckResult is one wal_checkpoint's row: busy, frames in the WAL, frames
// copied back (-1, -1 when not in WAL mode).
type ckResult struct{ busy, log, copied int64 }

func (r ckResult) complete() bool { return r.busy == 0 && r.log >= 0 && r.copied >= r.log }

// startCheckpointer starts the coordinator. Only for a WAL database: a
// database in another journal mode has no WAL to keep small.
func (l *lanes) startCheckpointer(pageSize int64) {
	if pageSize <= 0 {
		pageSize = 4096
	}
	ck := &checkpointer{every: checkpointEvery, idle: idleAfter, holdAt: walHoldAt, restartAt: walRestartAt,
		pageSize: pageSize, stop: make(chan struct{}), done: make(chan struct{})}
	l.ck = ck
	go l.checkpointLoop(ck)
}

// stopCheckpointer stops the coordinator and waits for it.
func (l *lanes) stopCheckpointer() {
	ck := l.ck
	if ck == nil {
		return
	}
	ck.once.Do(func() { close(ck.stop) })
	<-ck.done
}

func (l *lanes) checkpointLoop(ck *checkpointer) {
	defer close(ck.done)
	t := time.NewTicker(ck.every)
	defer t.Stop()
	var seen uint64
	truncated := false
	for {
		select {
		case <-ck.stop:
			return
		case <-t.C:
		}
		if n := l.commits.Load(); n != seen {
			seen = n
			truncated = false
			l.checkpointAfterCommits(ck)
			continue
		}
		if !truncated && time.Since(time.Unix(0, l.active.Load())) >= ck.idle {
			// Once per idle spell, done or not: the next commit allows another.
			l.truncateIdle()
			truncated = true
		}
	}
}

// checkpoint runs PRAGMA wal_checkpoint(mode) on W, holding W's admission
// at lane: Background for the routine PASSIVE, laneHold when the caller
// already holds W.
func (l *lanes) checkpoint(ctx context.Context, mode string, holdingW bool) (ckResult, error) {
	sql := "PRAGMA wal_checkpoint(" + mode + ")"
	run := func() (ckResult, error) {
		t := time.Now()
		rows, err := l.w.db.Query(ctx, sql)
		if perftrace.SQLOn() {
			perftrace.SQL(ctx, "ck@W", sql, t)
		}
		if isCode(err, sqliteBusy) {
			return ckResult{busy: 1}, nil
		}
		if err != nil {
			return ckResult{}, err
		}
		if len(rows.Values) != 1 || len(rows.Values[0]) < 3 {
			return ckResult{}, fmt.Errorf("store: unexpected wal_checkpoint result")
		}
		v := rows.Values[0]
		return ckResult{busy: Int(v[0]), log: Int(v[1]), copied: Int(v[2])}, nil
	}
	if holdingW {
		return run()
	}
	c, err := l.wq.acquire(ctx, Background, anyConn)
	if err != nil {
		return ckResult{}, err
	}
	defer l.wq.release(c)
	return run()
}

// checkpointAfterCommits is the coordinator's step after W committed.
func (l *lanes) checkpointAfterCommits(ck *checkpointer) {
	ctx := context.Background()
	r, err := l.checkpoint(ctx, "PASSIVE", false)
	if err != nil || r.complete() || r.log*ck.pageSize <= ck.holdAt {
		return
	}
	rs := l.readers.Load()
	if rs == nil {
		return
	}
	// Hold B for one statement: the coordinator goes first in its queue
	// and waits for the statement running there.
	hctx, cancel := context.WithTimeout(ctx, holdWait)
	b, err := rs.acquire(hctx, laneHold, func(c *conn) bool { return c == rs.b })
	cancel()
	if err != nil {
		return
	}
	for i := 0; i < holdTries; i++ {
		if r, err = l.checkpoint(ctx, "PASSIVE", false); err != nil || r.complete() {
			break
		}
		time.Sleep(holdPause)
	}
	if err == nil && !r.complete() && r.log*ck.pageSize > ck.restartAt {
		r = l.restart(ctx)
	}
	rs.release(b)
}

// restart is the safety net past walRestartAt, with B held by the caller:
// it holds W too, so no write appends meanwhile, and tries RESTART until
// the interactive readers' snapshots have moved past the WAL.
func (l *lanes) restart(ctx context.Context) ckResult {
	hctx, cancel := context.WithTimeout(ctx, holdWait)
	w, err := l.wq.acquire(hctx, laneHold, anyConn)
	cancel()
	if err != nil {
		return ckResult{busy: 1}
	}
	defer l.wq.release(w)
	start := time.Now()
	var r ckResult
	for time.Since(start) < restartFor {
		if r, err = l.checkpoint(ctx, "RESTART", true); err != nil || r.complete() {
			break
		}
		time.Sleep(holdPause)
	}
	slog.Default().Warn("the WAL passed its hard limit; checkpoint RESTART with the writes held",
		"wal_mb", r.log*l.ck.pageSize>>20, "done", r.complete(), "took", time.Since(start).Round(time.Millisecond), "lanes", l.busySummary())
	return r
}

// truncateIdle empties the WAL file when nothing has run for a while: it
// holds every connection (they are idle) and runs TRUNCATE. It reports
// whether the file was emptied.
func (l *lanes) truncateIdle() bool {
	ctx, cancel := context.WithTimeout(context.Background(), holdWait)
	defer cancel()
	var held []*conn
	rs := l.readers.Load()
	if rs != nil {
		for _, c := range rs.conns {
			got, err := rs.acquire(ctx, laneHold, func(x *conn) bool { return x == c })
			if err != nil {
				for _, h := range held {
					rs.release(h)
				}
				return false
			}
			held = append(held, got)
		}
	}
	defer func() {
		for _, h := range held {
			rs.release(h)
		}
	}()
	w, err := l.wq.acquire(ctx, laneHold, anyConn)
	if err != nil {
		return false
	}
	defer l.wq.release(w)
	r, err := l.checkpoint(context.Background(), "TRUNCATE", true)
	return err == nil && r.complete()
}

// busySummary is each connection's run time and busy-handler counters
// since Open, for the safety net's warning.
func (l *lanes) busySummary() string {
	s := ""
	cs := []*conn{l.w}
	if rs := l.readers.Load(); rs != nil {
		cs = append(cs, rs.conns...)
	}
	for _, c := range cs {
		s += fmt.Sprintf("%s ran %s, met SQLITE_BUSY %d times (%s); ", c.name, time.Duration(c.execNanos.Load()).Round(time.Millisecond),
			c.busyWaits.Load(), time.Duration(c.busyNanos.Load()).Round(time.Millisecond))
	}
	return s
}
