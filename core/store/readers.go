package store

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/egoist/mygo/plugins/sqlite"
	"github.com/godwit-email/godwit/internal/perftrace"
)

// The interface's reads on read-only connections beside the writer
// (ADR 0052).
//
// mygo's sqlite.DB runs one operation at a time, so on a single connection
// the interface's sub-millisecond reads waited behind every long statement
// before them: a sync round's batch, the labeler's scan, a saved view's
// count. The database is in WAL mode, where readers on other connections
// run beside the writer and see every committed transaction, so DB.Query
// sends the interface's plain SELECTs (a ctx marked by WithUIRead) to two
// read-only connections. Everything else stays on the writer: writes,
// PRAGMAs, WITH, RETURNING, connection-bound functions, and every read of
// the backend's own work, which keeps the one connection it always had and
// so takes no more CPU from the interface than before.
//
// mygo has no interactive transaction (Transaction is one batch under
// BEGIN IMMEDIATE, and Query refuses BEGIN), so a write has committed by the
// time its call returns and a read started after it, on any connection,
// sees it.

const (
	// readerCount is the pool's size: reader 0 is the interface's quick
	// lane, reader 1 its long lane (acquire).
	readerCount = 2
	// slowRead is the run time from which an unmarked statement counts as
	// long: its next runs stay off the quick lane.
	slowRead = 250 * time.Millisecond
	// maxTimed bounds the table of statement run times.
	maxTimed = 256
	// readerBusyTimeout bounds a reader's wait for a lock. In WAL a reader
	// waits only while the WAL index is being rebuilt or reset.
	readerBusyTimeout = 5 * time.Second
	// readerCacheKiB is PRAGMA cache_size on each reader, in KiB (SQLite's
	// default is 2,000). Under a sync the writer changes the database every
	// few milliseconds and a reader drops its cache at each change, so a
	// larger one holds memory, not hits; the OS page cache serves the rest.
	readerCacheKiB = 1024
)

type uiReadKey struct{}

// uiMark is WithUIRead's value: the life of the interface's connection,
// or off (WithoutUIRead).
type uiMark struct {
	done <-chan struct{} // closed when the connection ends; nil: never
	off  bool
}

// WithUIRead marks ctx as serving Godwit's own interface: its plain
// SELECTs run on the read-only connections and never wait for the
// backend's writes or reads. The core marks every RoleUI connection's
// context. The mark lasts as long as ctx: work detached from it with
// context.WithoutCancel keeps the mark while the connection lives and
// reads on the writer once it has ended.
func WithUIRead(ctx context.Context) context.Context {
	return context.WithValue(ctx, uiReadKey{}, uiMark{done: ctx.Done()})
}

// WithoutUIRead takes the mark off ctx: work an interface call starts
// but that is the backend's own, such as a Wit run, reads on the writer.
func WithoutUIRead(ctx context.Context) context.Context {
	if !isUIRead(ctx) {
		return ctx
	}
	return context.WithValue(ctx, uiReadKey{}, uiMark{off: true})
}

func isUIRead(ctx context.Context) bool {
	m, ok := ctx.Value(uiReadKey{}).(uiMark)
	if !ok || m.off {
		return false
	}
	select {
	case <-m.done:
		return false
	default:
		return true
	}
}

type longReadKey struct{}

// WithLongRead marks ctx's reads as scans (a saved view's placement, the
// reply statuses, the contacts): on the interface's connections they take
// the long lane only, so they never hold the quick lane that a thread or a
// list waits for, from their first run on.
func WithLongRead(ctx context.Context) context.Context {
	return context.WithValue(ctx, longReadKey{}, true)
}

func isLongRead(ctx context.Context) bool {
	long, _ := ctx.Value(longReadKey{}).(bool)
	return long
}

type foregroundScanKey struct{}

// WithForegroundScan marks ctx's reads as a scan that the user waits on
// now, such as the reply statuses behind the deadline a reader message
// shows: on the interface's connections they never queue behind the long
// lane's scans. They take the long lane when it is free, else the quick
// lane when it is free, else whichever frees first; their run time never
// sends them to the long lane alone. WithLongRead wins over it.
func WithForegroundScan(ctx context.Context) context.Context {
	return context.WithValue(ctx, foregroundScanKey{}, true)
}

func isForegroundScan(ctx context.Context) bool {
	fg, _ := ctx.Value(foregroundScanKey{}).(bool)
	return fg
}

// reader is one read-only connection and its lane: a one-slot semaphore
// held while a statement runs, so a caller can see the connection is busy
// without queueing on it.
type reader struct {
	db   *sqlite.DB
	lane chan struct{}
	kind string // the SQL trace's kind: "q@r0", "q@r1"
}

// readers is DB's pool. It opens on the first routed read, not in Open:
// a caller that never reads for the interface, such as the dev database
// copy that switches to journal_mode = DELETE (core/devsync.go), which
// needs no other connection open, never has readers.
type readers struct {
	path string
	pool atomic.Pointer[[]*reader]
	mu   sync.Mutex // opening and closing
	off  bool       // closed, or opening failed: every read stays on the writer

	timeMu sync.Mutex
	timed  map[string]bool // statement text: its last run took slowRead or more
}

// readClass is what acquire knows of an interface read.
type readClass int

const (
	quickRead      readClass = iota // not marked, and its last run (if any) under slowRead
	longRead                        // marked by WithLongRead, or its last run slowRead or more
	foregroundScan                  // marked by WithForegroundScan, not by WithLongRead
)

// class is ctx and sql's readClass. A statement not run yet is quick
// unless marked: the scans the interface starts are marked, so at a cold
// start, when no statement has a run time, they still keep off the quick
// lane.
func (rs *readers) class(ctx context.Context, sql string) readClass {
	if isLongRead(ctx) {
		return longRead
	}
	if isForegroundScan(ctx) {
		return foregroundScan
	}
	rs.timeMu.Lock()
	slow := rs.timed[sql]
	rs.timeMu.Unlock()
	if slow {
		return longRead
	}
	return quickRead
}

// ran records how long sql ran. The table is cleared when full: its
// statements are then new again until they run once more.
func (rs *readers) ran(sql string, d time.Duration) {
	rs.timeMu.Lock()
	defer rs.timeMu.Unlock()
	if _, ok := rs.timed[sql]; !ok && len(rs.timed) >= maxTimed {
		rs.timed = nil
	}
	if rs.timed == nil {
		rs.timed = make(map[string]bool)
	}
	rs.timed[sql] = d >= slowRead
}

// get returns the pool, opening it the first time; nil means the writer.
func (rs *readers) get() []*reader {
	if p := rs.pool.Load(); p != nil {
		return *p
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if p := rs.pool.Load(); p != nil {
		return *p
	}
	if rs.off {
		return nil
	}
	pool, err := openReaders(rs.path)
	if err != nil {
		rs.off = true
		slog.Default().Warn("could not open read-only database connections; reads share the writer", "err", err)
		return nil
	}
	rs.pool.Store(&pool)
	return pool
}

func openReaders(path string) ([]*reader, error) {
	// Not the caller's ctx: a canceled first read must not turn the pool off.
	ctx := context.Background()
	pool := make([]*reader, 0, readerCount)
	fail := func(err error) ([]*reader, error) {
		for _, r := range pool {
			r.db.Close()
		}
		return nil, err
	}
	for i := range readerCount {
		db, err := sqlite.Open(ctx, path, sqlite.OpenOptions{ReadOnly: true, BusyTimeout: readerBusyTimeout})
		if err != nil {
			return fail(err)
		}
		r := &reader{db: db, lane: make(chan struct{}, 1), kind: "q@r" + strconv.Itoa(i)}
		r.lane <- struct{}{}
		pool = append(pool, r)
		if _, err := db.Query(ctx, "PRAGMA cache_size = -"+strconv.Itoa(readerCacheKiB)); err != nil {
			return fail(err)
		}
	}
	return pool, nil
}

// close closes the readers and keeps later reads on the writer.
func (rs *readers) close() error {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.off = true
	p := rs.pool.Swap(nil)
	if p == nil {
		return nil
	}
	var err error
	for _, r := range *p {
		err = errors.Join(err, r.db.Close())
	}
	return err
}

// acquire takes a reader for a read of class c and returns it; the
// caller gives its lane back.
//
// A long read waits for the long lane (1) alone: saved views and
// contacts run statements of tenths of seconds to seconds, and the window
// restarts SavedViews on every mail event during a sync. A quick read
// takes the quick lane (0) first, the long lane when it is free, and
// otherwise whichever frees first, so it waits at most for one quick
// statement or for the long one in progress, never for a queue of scans.
// A foreground scan (a reader message's ThreadDeadline) is the same with
// the lanes swapped: it prefers the long lane so that the quick one stays
// free for the next thread, but it never queues behind the long lane's
// scans, which arrive faster than they drain during a sync.
func acquire(ctx context.Context, pool []*reader, c readClass) (*reader, error) {
	quick, long := pool[0], pool[1]
	if c == longRead {
		select {
		case <-long.lane:
			return long, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	order := []*reader{quick, long}
	if c == foregroundScan {
		order = []*reader{long, quick}
	}
	for _, r := range order {
		select {
		case <-r.lane:
			return r, nil
		default:
		}
	}
	select {
	case <-quick.lane:
		return quick, nil
	case <-long.lane:
		return long, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// acquire names the two readers: a compile error here means it must
// change with readerCount.
var _ = [1]struct{}{}[readerCount-2]

// read runs sql on a reader, timed for the SQL trace by the reader that
// ran it (its wait for a lane included).
func (rs *readers) read(ctx context.Context, pool []*reader, sql string, args []any) (sqlite.Rows, error) {
	t := time.Now()
	r, err := acquire(ctx, pool, rs.class(ctx, sql))
	if err != nil {
		return sqlite.Rows{}, err
	}
	run := time.Now()
	rows, err := r.db.Query(ctx, sql, args...)
	r.lane <- struct{}{}
	// A statement canceled after slowRead is long all the same.
	if d := time.Since(run); err == nil || d >= slowRead {
		rs.ran(sql, d)
	}
	if perftrace.SQLOn() {
		perftrace.SQL(ctx, r.kind, sql, t)
	}
	return rows, err
}

// readOnlySQL reports whether sql can run on a read-only connection: its
// first keyword, after blanks and comments, is SELECT, and it calls none of
// the functions that read the writer's connection state. WITH stays on the
// writer, since a write can follow it, and so does anything unrecognized;
// a write that still reaches a reader fails there, read-only.
func readOnlySQL(sql string) bool {
	s := skipBlank(sql)
	if len(s) < 6 || !strings.EqualFold(s[:6], "select") || (len(s) > 6 && identByte(s[6])) {
		return false
	}
	low := strings.ToLower(s)
	for _, fn := range []string{"last_insert_rowid", "changes", "total_changes"} {
		if callsFunc(low, fn) {
			return false
		}
	}
	return true
}

// skipBlank drops leading white space and SQL comments.
func skipBlank(s string) string {
	for {
		s = strings.TrimLeft(s, " \t\r\n\f\v")
		switch {
		case strings.HasPrefix(s, "--"):
			i := strings.IndexByte(s, '\n')
			if i < 0 {
				return ""
			}
			s = s[i+1:]
		case strings.HasPrefix(s, "/*"):
			i := strings.Index(s[2:], "*/")
			if i < 0 {
				return ""
			}
			s = s[i+4:]
		default:
			return s
		}
	}
}

// callsFunc reports whether low (lower case) has name as a word followed,
// after optional blanks, by "(". A column of that name followed by "("
// would match too: that read just stays on the writer.
func callsFunc(low, name string) bool {
	for i := 0; ; {
		j := strings.Index(low[i:], name)
		if j < 0 {
			return false
		}
		start, end := i+j, i+j+len(name)
		i = end
		if start > 0 && identByte(low[start-1]) {
			continue
		}
		if rest := strings.TrimLeft(low[end:], " \t\r\n\f\v"); strings.HasPrefix(rest, "(") {
			return true
		}
	}
}

func identByte(c byte) bool {
	return c == '_' || c == '$' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}
