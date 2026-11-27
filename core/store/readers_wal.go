package store

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// Keeping the WAL small beside the readers (ADR 0052).
//
// The WAL restarts from its beginning only after a checkpoint has copied
// every frame back into the database, and a checkpoint stops at the
// oldest snapshot a reader holds. During a sync the writer commits every
// few milliseconds while the interface's two readers run statements of
// tenths of seconds to seconds back to back, so some reader nearly always
// holds a snapshot older than the last commit: the WAL never restarts and
// grows with everything the sync writes. perfbench measured 1.2 GB after
// 27 s (the single connection: 23 MB), and the file keeps its largest
// size. So when the WAL passes walLimit, the backend's writes wait while
// a drain empties the readers and truncates the WAL:
//
//   - new interface reads, and those still waiting for a lane, read on
//     the writer at once, which the paused writes leave free; long reads
//     (WithLongRead or slow) wait for the drain's end instead, since a
//     saved view's count or a sender scan on the writer would hold the
//     reads the user waits for;
//   - the drain takes both lanes, so it waits only for the statements the
//     readers are running;
//   - then PRAGMA wal_checkpoint(TRUNCATE) on the writer copies the WAL
//     back and empties the file, and everything resumes.
//
// The interface's own writes never wait: they are small, and the user
// waits for them. If the readers do not empty within drainWait, the
// writes resume and the next drain comes drainRetry later; so does it when
// the checkpoint could not empty the WAL (another process reading it).

const (
	// walLimit is the WAL size from which the backend's writes wait for a
	// drain. On the single connection the WAL stayed under 25 MB during a
	// sync.
	walLimit = 64 << 20
	// walCheckEvery spaces the WAL size checks of the backend's writes.
	walCheckEvery = 100 * time.Millisecond
	// drainWait bounds the wait for the readers' statements.
	drainWait = 30 * time.Second
	// drainRetry spaces drains after one that timed out.
	drainRetry = 5 * time.Second
)

// errDraining tells Query that a drain runs: it reads on the writer.
var errDraining = errors.New("store: readers draining")

// walGuard is readers' WAL state.
type walGuard struct {
	draining   atomic.Bool                 // a drain runs: interface reads take the writer
	checkpoint func(context.Context) error // on the writer; set by Open
	limit      int64                       // walLimit, or a test's
	mu         sync.Mutex                  // the fields below
	done       chan struct{}               // closed when the running drain ends; nil: none runs
	start      chan struct{}               // closed when a drain starts; nil: made by started
	next       time.Time                   // no size check before
}

// running returns the running drain's channel, closed when it ends; nil
// when none runs.
func (g *walGuard) running() <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.done
}

// started returns a channel closed once a drain runs: the next one's, or
// the running one's, already closed. acquire stops waiting on it.
func (g *walGuard) started() <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.start == nil {
		g.start = make(chan struct{})
	}
	return g.start
}

// beforeWrite makes a write of the backend's wait for the drain that
// runs, starting one if the WAL has passed its limit. Writes made while
// no reader is open, and the interface's own writes, never wait.
func (rs *readers) beforeWrite(ctx context.Context) {
	if isUIRead(ctx) {
		return
	}
	p := rs.pool.Load()
	if p == nil {
		return
	}
	if done := rs.drainFor(*p); done != nil {
		select {
		case <-done:
		case <-ctx.Done():
		}
	}
}

// drainFor returns the running drain's channel, first starting a drain if
// the WAL has passed its limit; nil when none runs.
func (rs *readers) drainFor(pool []*reader) <-chan struct{} {
	g := &rs.wal
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.done != nil {
		return g.done
	}
	now := time.Now()
	if now.Before(g.next) {
		return nil
	}
	g.next = now.Add(walCheckEvery)
	if !rs.walOver(g.limitOr()) {
		return nil
	}
	g.done = make(chan struct{})
	g.draining.Store(true)
	if g.start == nil {
		g.start = make(chan struct{})
	}
	close(g.start) // the reads waiting for a lane go to the writer
	go rs.drain(pool, g.done)
	return g.done
}

// drain takes every lane, truncates the WAL on the writer and gives the
// lanes back (walGuard).
func (rs *readers) drain(pool []*reader, done chan struct{}) {
	g := &rs.wal
	ctx, cancel := context.WithTimeout(context.Background(), drainWait)
	defer cancel()
	var held []*reader
	for _, r := range pool {
		select {
		case <-r.lane:
			held = append(held, r)
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
	}
	emptied := len(held) == len(pool)
	if emptied {
		if err := g.checkpoint(ctx); err != nil {
			slog.Default().Debug("WAL checkpoint after draining the readers failed", "err", err)
		}
	} else {
		slog.Default().Debug("readers still busy: WAL not truncated", "wait", drainWait)
	}
	g.mu.Lock()
	g.draining.Store(false)
	g.done = nil
	g.start = nil
	if !emptied || rs.walOver(g.limitOr()) {
		g.next = time.Now().Add(drainRetry)
	}
	g.mu.Unlock()
	for _, r := range held {
		r.lane <- struct{}{}
	}
	close(done)
}

func (g *walGuard) limitOr() int64 {
	if g.limit != 0 {
		return g.limit
	}
	return walLimit
}

// walOver reports whether the WAL file has reached limit bytes.
func (rs *readers) walOver(limit int64) bool {
	fi, err := os.Stat(rs.path + "-wal")
	return err == nil && fi.Size() >= limit
}
