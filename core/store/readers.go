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

// Reads on read-only connections beside the writer (ADR 0052).
//
// mygo's sqlite.DB runs one operation at a time, so on a single connection
// a sub-millisecond read waits behind every long statement before it: a
// saved view's scan, a sync round's whole-folder SELECT. The database is in
// WAL mode, where readers on other connections run beside the writer and
// see every committed transaction, so DB.Query sends plain SELECTs to a
// small pool of read-only connections. Everything else (writes, PRAGMAs,
// WITH, RETURNING, connection-bound functions) stays on the writer.
//
// mygo has no interactive transaction (Transaction is one batch under
// BEGIN IMMEDIATE, and Query refuses BEGIN), so a write has committed by the
// time its call returns and a read started after it, on any connection,
// sees it.

const (
	// readerCount is the pool's size. Readers 0..uiReaders-1 serve only
	// the interface; the last one is shared: background reads queue on it
	// alone, and an interface read takes it when it is free. The interface
	// itself starts overlapping long reads (a refresh's SavedViews while
	// the previous one runs), so it gets two lanes of its own before it
	// borrows the shared one.
	readerCount = 3
	uiReaders   = readerCount - 1
	// readerBusyTimeout bounds a reader's wait for a lock. In WAL a reader
	// waits only while the WAL index is being rebuilt or reset.
	readerBusyTimeout = 5 * time.Second
	// readerMmapSize is PRAGMA mmap_size on the readers (0: off).
	readerMmapSize = 0
)

type uiReadKey struct{}

// WithUIRead marks ctx as serving Godwit's own interface: its reads take
// the interface's reader lanes and never queue behind background reads
// while a lane is free. The core marks every RoleUI connection's context.
func WithUIRead(ctx context.Context) context.Context {
	return context.WithValue(ctx, uiReadKey{}, true)
}

func isUIRead(ctx context.Context) bool {
	ui, _ := ctx.Value(uiReadKey{}).(bool)
	return ui
}

// reader is one read-only connection and its lane: a one-slot semaphore
// held while a statement runs, so a caller can see the connection is busy
// without queueing on it.
type reader struct {
	db   *sqlite.DB
	lane chan struct{}
	kind string // the SQL trace's kind: "q@r0", "q@r1", ...
}

// readers is DB's pool. It opens on the first routed read, not in Open:
// a caller that never reads through DB.Query, such as the dev database
// copy that switches to journal_mode = DELETE (core/devsync.go), which
// needs no other connection open, never has readers.
type readers struct {
	path string
	pool atomic.Pointer[[]*reader]
	mu   sync.Mutex // opening and closing
	off  bool       // closed, or opening failed: every read stays on the writer
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
		if readerMmapSize > 0 {
			if _, err := db.Query(ctx, "PRAGMA mmap_size = "+strconv.Itoa(readerMmapSize)); err != nil {
				return fail(err)
			}
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

// acquire takes a reader for ctx: a background read waits for the shared
// reader; an interface read takes the first free reader, else waits for
// whichever frees first. The caller gives the lane back.
func acquire(ctx context.Context, pool []*reader) (*reader, error) {
	shared := pool[len(pool)-1]
	if !isUIRead(ctx) {
		select {
		case <-shared.lane:
			return shared, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	for _, r := range pool {
		select {
		case <-r.lane:
			return r, nil
		default:
		}
	}
	select {
	case <-pool[0].lane:
		return pool[0], nil
	case <-pool[1].lane:
		return pool[1], nil
	case <-shared.lane:
		return shared, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// acquire's last select names the three readers: a compile error here
// means it must change with readerCount.
var _ = [1]struct{}{}[readerCount-3]

// read runs sql on a reader, timed for the SQL trace by the reader that
// ran it (its wait for a lane included).
func (rs *readers) read(ctx context.Context, pool []*reader, sql string, args []any) (sqlite.Rows, error) {
	t := time.Now()
	r, err := acquire(ctx, pool)
	if err != nil {
		return sqlite.Rows{}, err
	}
	rows, err := r.db.Query(ctx, sql, args...)
	r.lane <- struct{}{}
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
