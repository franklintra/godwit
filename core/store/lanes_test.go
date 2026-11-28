package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/egoist/mygo/plugins/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func openLanesDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(t.Context(), filepath.Join(t.TempDir(), "godwit.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	_, err = db.Execute(t.Context(), "CREATE TABLE IF NOT EXISTS t(x INTEGER)")
	require.NoError(t, err)
	return db
}

func readersOf(t *testing.T, db *DB) *readerSet {
	t.Helper()
	rs := db.lanes.readerSet()
	require.NotNil(t, rs)
	return rs
}

// traceReaders records the statements that reach a reader until the test
// ends.
func traceReaders(t *testing.T, db *DB) func() []string {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	db.lanes.onReader = func(conn, sql string) {
		mu.Lock()
		seen = append(seen, conn+" "+sql)
		mu.Unlock()
	}
	t.Cleanup(func() { db.lanes.onReader = nil })
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

func lane(ctx context.Context, l Lane) context.Context { return WithLane(ctx, l, "Test") }

func TestReadOnlySQL(t *testing.T) {
	for sql, want := range map[string]bool{
		"SELECT 1": true,
		"  \n\tselect id FROM messages WHERE id = ?":                               true,
		"-- note\nSELECT 1":                                                        true,
		"/* a */ /* b */SELECT 1":                                                  true,
		"SELECT (SELECT page_size FROM pragma_page_size)":                          true,
		"SELECT changes_at FROM t":                                                 true, // a column, not the function
		"SELECT x FROM t WHERE label = 'changes'":                                  true,
		"SELECT count(*) FROM (WITH RECURSIVE c(x) AS (SELECT 1) SELECT x FROM c)": true,
		"PRAGMA user_version":                                                      false,
		"PRAGMA synchronous = FULL":                                                false,
		"PRAGMA wal_checkpoint(PASSIVE)":                                           false,
		"PRAGMA incremental_vacuum(64)":                                            false,
		"WITH c AS (SELECT 1) SELECT * FROM c":                                     false,
		"WITH c AS (SELECT 1) DELETE FROM t":                                       false,
		"INSERT INTO t(x) VALUES (1) RETURNING x":                                  false,
		"UPDATE t SET x = 1 RETURNING x":                                           false,
		"DELETE FROM t RETURNING x":                                                false,
		"SELECT last_insert_rowid()":                                               false,
		"SELECT changes()":                                                         false,
		"SELECT total_changes ()":                                                  false,
		"select CHANGES ( )":                                                       false,
		"SELECTx":                                                                  false,
		"VALUES (1)":                                                               false,
		"-- only a comment":                                                        false,
		"/* unterminated SELECT 1":                                                 false,
		"":                                                                         false,
	} {
		assert.Equal(t, want, readOnlySQL(sql), sql)
	}
	for sql, want := range map[string]bool{
		"PRAGMA journal_mode = DELETE":   true,
		"pragma main.journal_mode=wal":   true,
		"PRAGMA journal_mode":            false,
		"SELECT 'journal_mode = DELETE'": false,
	} {
		assert.Equal(t, want, setsJournalMode(sql), sql)
	}
}

// Statements go where their text and their caller's lane say.
func TestRouting(t *testing.T) {
	db := openLanesDB(t)
	seen := traceReaders(t, db)
	ctx := t.Context()
	for _, tc := range []struct {
		ctx  context.Context
		sql  string
		conn string // "" = the writer
	}{
		{lane(ctx, Interactive), "SELECT count(*) FROM t", "I0"},
		{lane(ctx, UIBackground), "SELECT count(*) FROM t", "B"},
		{lane(ctx, Background), "SELECT count(*) FROM t", "B"},
		{ctx, "SELECT count(*) FROM t", "B"}, // no mark: Background
		{lane(ctx, Interactive), "PRAGMA user_version", ""},
		{lane(ctx, Interactive), "WITH c AS (SELECT 1) SELECT * FROM c", ""},
		{lane(ctx, Interactive), "SELECT last_insert_rowid()", ""},
		{lane(ctx, Interactive), "INSERT INTO t(x) VALUES (1) RETURNING x", ""},
	} {
		before := len(seen())
		_, err := db.Query(tc.ctx, tc.sql)
		require.NoError(t, err, tc.sql)
		got := seen()[before:]
		if tc.conn == "" {
			assert.Empty(t, got, tc.sql)
			continue
		}
		require.Len(t, got, 1, tc.sql)
		assert.Equal(t, tc.conn+" "+tc.sql, got[0])
	}
}

// The readers open read-only: a write that reaches one fails there.
func TestMisroutedWriteFails(t *testing.T) {
	db := openLanesDB(t)
	rs := readersOf(t, db)
	for _, c := range rs.conns {
		_, err := c.db.Execute(t.Context(), "INSERT INTO t(x) VALUES (1)")
		var se *sqlite.Error
		require.ErrorAs(t, err, &se, c.name)
		assert.Equal(t, sqliteReadOnly, se.Code, c.name)
	}
	rows, err := db.Query(t.Context(), "SELECT count(*) FROM t")
	require.NoError(t, err)
	assert.EqualValues(t, 0, Int(rows.Values[0][0]))
}

func TestReaderSettings(t *testing.T) {
	db := openLanesDB(t)
	for _, c := range readersOf(t, db).conns {
		rows, err := c.db.Query(t.Context(), "PRAGMA cache_size")
		require.NoError(t, err)
		assert.EqualValues(t, -readerCacheKiB, Int(rows.Values[0][0]), c.name)
		rows, err = c.db.Query(t.Context(), "PRAGMA mmap_size")
		require.NoError(t, err)
		assert.EqualValues(t, 0, Int(rows.Values[0][0]), c.name)
	}
	rows, err := db.DB.Query(t.Context(), "PRAGMA journal_size_limit")
	require.NoError(t, err)
	assert.EqualValues(t, walSizeLimit, Int(rows.Values[0][0]))
}

// A write committed on the writer by another goroutine is seen by the next
// read on every connection.
func TestReadAfterWrite(t *testing.T) {
	db := openLanesDB(t)
	rs := readersOf(t, db)
	for i := range 20 {
		done := make(chan error)
		go func() {
			_, err := db.Execute(context.Background(), "INSERT INTO t(x) VALUES (?)", i)
			done <- err
		}()
		require.NoError(t, <-done)
		for _, c := range rs.conns {
			rows, err := c.db.Query(t.Context(), "SELECT count(*) FROM t")
			require.NoError(t, err)
			assert.EqualValues(t, i+1, Int(rows.Values[0][0]), c.name)
		}
		for _, l := range []Lane{Interactive, UIBackground, Background} {
			var wg sync.WaitGroup
			wg.Go(func() {
				rows, err := db.Query(lane(context.Background(), l), "SELECT count(*) FROM t")
				assert.NoError(t, err)
				assert.EqualValues(t, i+1, Int(rows.Values[0][0]), l.String())
			})
			wg.Wait()
		}
	}
}

func fakeConns(names ...string) (*sched, map[string]*conn) {
	s := &sched{}
	m := map[string]*conn{}
	for _, n := range names {
		k := connI
		if n == "B" {
			k = connB
		}
		c := &conn{name: n, kind: k}
		s.conns = append(s.conns, c)
		m[n] = c
	}
	return s, m
}

// waitQueued waits until n callers are queued in s.
func waitQueued(t *testing.T, s *sched, n int) {
	t.Helper()
	require.Eventually(t, func() bool { return s.queued() == n }, 2*time.Second, time.Millisecond)
}

// B serves its waiters by lane, FIFO within one; Interactive borrows an
// idle B and never waits for it while I is free; Background never takes I.
func TestSchedPriorityAndBorrowing(t *testing.T) {
	ctx := t.Context()
	s, c := fakeConns("I0", "I1", "B")
	b, err := s.acquire(ctx, Background, wantFor(Background))
	require.NoError(t, err)
	require.Equal(t, c["B"], b)

	order := make(chan string, 8)
	var wg sync.WaitGroup
	queue := func(name string, l Lane) {
		wg.Go(func() {
			got, err := s.acquire(ctx, l, wantFor(l))
			assert.NoError(t, err)
			order <- name + ":" + got.name
			time.Sleep(5 * time.Millisecond)
			s.release(got)
		})
	}
	queue("bg1", Background)
	waitQueued(t, s, 1)
	queue("bg2", Background)
	waitQueued(t, s, 2)
	queue("ui1", UIBackground)
	waitQueued(t, s, 3)
	// Both I connections are free: Interactive takes them at once.
	i0, err := s.acquire(ctx, Interactive, wantFor(Interactive))
	require.NoError(t, err)
	i1, err := s.acquire(ctx, Interactive, wantFor(Interactive))
	require.NoError(t, err)
	assert.Equal(t, "I0 I1", i0.name+" "+i1.name)
	// Now an interactive waiter queues for any reader, ahead of B's queue.
	queue("int", Interactive)
	waitQueued(t, s, 4)
	s.release(b)
	wg.Wait()
	close(order)
	var got []string
	for o := range order {
		got = append(got, o)
	}
	assert.Equal(t, []string{"int:B", "ui1:B", "bg1:B", "bg2:B"}, got)
	s.release(i0)
	s.release(i1)

	// I idle and B busy: a Background statement still waits for B.
	b, err = s.acquire(ctx, Background, wantFor(Background))
	require.NoError(t, err)
	short, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	_, err = s.acquire(short, Background, wantFor(Background))
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	s.release(b)
}

// A waiter whose ctx ends leaves the queue, and a connection handed to it
// as it ends goes to the next waiter.
func TestSchedCancelledWaiterLeaves(t *testing.T) {
	ctx := t.Context()
	s, _ := fakeConns("B")
	b, err := s.acquire(ctx, Background, anyConn)
	require.NoError(t, err)
	gone, cancel := context.WithCancel(ctx)
	errc := make(chan error, 1)
	go func() {
		_, err := s.acquire(gone, Interactive, anyConn)
		errc <- err
	}()
	waitQueued(t, s, 1)
	cancel()
	assert.ErrorIs(t, <-errc, context.Canceled)
	assert.Equal(t, 0, s.queued())
	s.release(b)
	b, err = s.acquire(ctx, Background, anyConn)
	require.NoError(t, err, "the connection is free again")

	// Races of release and cancel never lose the connection.
	for range 200 {
		gone, cancel := context.WithCancel(ctx)
		go func() {
			if c, err := s.acquire(gone, Interactive, anyConn); err == nil {
				s.release(c)
			}
		}()
		waitQueued(t, s, 1)
		go cancel()
		s.release(b)
		b, err = s.acquire(ctx, Background, anyConn)
		require.NoError(t, err)
		cancel()
	}
	s.release(b)
}

// A Background statement waits for interactive calls in flight, at most
// backgroundYield; with none in flight, it does not wait.
func TestYieldIsBounded(t *testing.T) {
	start := time.Now()
	yieldToUI(t.Context())
	assert.Less(t, time.Since(start), 10*time.Millisecond)

	call, end := context.WithCancel(t.Context())
	WithLane(call, Interactive, "GetThread")
	start = time.Now()
	yieldToUI(t.Context())
	d := time.Since(start)
	assert.GreaterOrEqual(t, d, backgroundYield-5*time.Millisecond)
	assert.Less(t, d, backgroundYield+40*time.Millisecond, "the yield never passes its bound (plus timer slack)")

	go func() { time.Sleep(10 * time.Millisecond); end() }()
	start = time.Now()
	yieldToUI(t.Context())
	assert.Less(t, time.Since(start), backgroundYield, "the call ended: the yield ends with it")

	// A detached copy of an ended call is Background.
	l, _ := LaneOf(context.WithoutCancel(WithLane(call, Interactive, "GetThread")))
	assert.Equal(t, Background, l)
}

func TestLaneOf(t *testing.T) {
	l, _ := LaneOf(t.Context())
	assert.Equal(t, Background, l)
	call, end := context.WithCancel(t.Context())
	ctx := WithLane(call, UIBackground, "SavedViews")
	l, m := LaneOf(context.WithoutCancel(ctx))
	assert.Equal(t, UIBackground, l, "detached work keeps the lane while the call runs")
	assert.Equal(t, "SavedViews", m)
	end()
	l, _ = LaneOf(context.WithoutCancel(ctx))
	assert.Equal(t, Background, l, "and drops to Background when it ends")
	for _, l := range []Lane{Background, UIBackground, Interactive} {
		got, ok := ParseLane(l.String())
		assert.True(t, ok)
		assert.Equal(t, l, got)
	}
	_, ok := ParseLane("hold")
	assert.False(t, ok)
}

// An interactive read is never queued behind a Background statement:
// while B and the writer run long background statements, a read on I
// answers at once.
func TestInteractiveSkipsBackground(t *testing.T) {
	db := openLanesDB(t)
	readersOf(t, db)
	hold := func(ctx context.Context, sql string, query bool) {
		t.Helper()
		ctx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			if query {
				db.Query(ctx, sql)
			} else {
				db.Execute(ctx, sql)
			}
		}()
		t.Cleanup(func() { cancel(); <-done })
	}
	const endless = "SELECT count(*) FROM (WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM c) SELECT x FROM c)"
	hold(context.Background(), endless, true)
	hold(context.Background(), "INSERT INTO t(x) SELECT x FROM (WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM c) SELECT x FROM c)", false)
	time.Sleep(50 * time.Millisecond)
	quick, cancel := context.WithTimeout(lane(t.Context(), Interactive), 2*time.Second)
	defer cancel()
	start := time.Now()
	_, err := db.Query(quick, "SELECT count(*) FROM t")
	require.NoError(t, err)
	assert.Less(t, time.Since(start), 500*time.Millisecond)
}

// PRAGMA synchronous (DurableTransaction's switch) runs on the writer only.
func TestSynchronousStaysOnWriter(t *testing.T) {
	db := openLanesDB(t)
	readersOf(t, db)
	seen := traceReaders(t, db)
	old := isTest
	isTest = func() bool { return false }
	t.Cleanup(func() { isTest = old })
	ctx := lane(t.Context(), Interactive)
	_, err := db.DurableExecute(ctx, "INSERT INTO t(x) VALUES (1)")
	require.NoError(t, err)
	_, err = db.DurableTransaction(ctx, []sqlite.Statement{{SQL: "INSERT INTO t(x) VALUES (2)"}})
	require.NoError(t, err)
	for _, s := range seen() {
		assert.NotContains(t, strings.ToLower(s), "pragma", s)
	}
	rows, err := db.DB.Query(t.Context(), "PRAGMA synchronous")
	require.NoError(t, err)
	assert.EqualValues(t, 1, Int(rows.Values[0][0]), "back to NORMAL")
}

// The dev copy leaves WAL (core/devsync.go): the readers close first, and
// every read after stays on the writer.
func TestJournalDeleteKeepsReadsOnWriter(t *testing.T) {
	db := openLanesDB(t)
	readersOf(t, db)
	seen := traceReaders(t, db)
	rows, err := db.Query(t.Context(), "PRAGMA journal_mode = DELETE")
	require.NoError(t, err)
	assert.Equal(t, "delete", Str(rows.Values[0][0]))
	for _, l := range []Lane{Interactive, UIBackground, Background} {
		_, err := db.Query(lane(t.Context(), l), "SELECT count(*) FROM t")
		require.NoError(t, err)
	}
	assert.Empty(t, seen())
	assert.Nil(t, db.lanes.readerSet())
}

// devsync's own path: no routed read before the switch, so no reader
// ever opens.
func TestNoReadersUntilARead(t *testing.T) {
	db := openLanesDB(t)
	_, err := db.Transaction(t.Context(), []sqlite.Statement{{SQL: "INSERT INTO t(x) VALUES (1)"}})
	require.NoError(t, err)
	assert.Nil(t, db.lanes.readers.Load())
	rows, err := db.DB.Query(t.Context(), "PRAGMA journal_mode = DELETE")
	require.NoError(t, err)
	assert.Equal(t, "delete", Str(rows.Values[0][0]))
}

// SQLITE_BUSY is retried with SQLite's pauses, counted, up to the
// connection's timeout, and stops at ctx's end.
func TestRetryBusy(t *testing.T) {
	l := newLanes("", nil)
	c := &conn{name: "B", busyTimeout: 200 * time.Millisecond}
	busy := &sqlite.Error{Code: sqliteBusy, ExtendedCode: sqliteBusy}
	n := 0
	waited, err := l.retryBusy(t.Context(), c, "SELECT 1", func() error {
		n++
		if n < 4 {
			return busy
		}
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, 4, n)
	assert.Positive(t, waited)
	assert.EqualValues(t, 1, c.busyWaits.Load())

	start := time.Now()
	_, err = l.retryBusy(t.Context(), c, "SELECT 1", func() error { return busy })
	assert.True(t, isCode(err, sqliteBusy))
	assert.Less(t, time.Since(start), 300*time.Millisecond)

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	c.busyTimeout = 5 * time.Second
	_, err = l.retryBusy(ctx, c, "SELECT 1", func() error { return busy })
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

// restartCheckpointer runs db's coordinator with shorter intervals.
func restartCheckpointer(db *DB, every, idle time.Duration) {
	db.lanes.stopCheckpointer()
	ck := &checkpointer{every: every, idle: idle, holdAt: walHoldAt, restartAt: walRestartAt,
		pageSize: 4096, stop: make(chan struct{}), done: make(chan struct{})}
	db.lanes.ck = ck
	go db.lanes.checkpointLoop(ck)
}

func walSize(path string) int64 {
	fi, err := os.Stat(path + "-wal")
	if err != nil {
		return 0
	}
	return fi.Size()
}

// The WAL stays bounded while a reader loops ~50 ms statements on B and
// the writer commits: the coordinator holds B for one statement and the
// checkpoint completes (perf/store-readers2 grew it to 1.2 GB).
func TestWALBoundedBesideReader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "godwit.db")
	db, err := Open(t.Context(), path)
	require.NoError(t, err)
	defer db.Close()
	ctx := t.Context()
	_, err = db.Execute(ctx, "CREATE TABLE big(id INTEGER PRIMARY KEY, b BLOB)")
	require.NoError(t, err)
	// Enough rows that a scan of them takes tens of milliseconds.
	_, err = db.Execute(ctx, "INSERT INTO big(b) SELECT randomblob(2000) FROM (WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM c WHERE x < 10000) SELECT x FROM c)")
	require.NoError(t, err)
	restartCheckpointer(db, 20*time.Millisecond, time.Hour)

	stop := make(chan struct{})
	var reads atomic.Int64
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			// About 50 ms, one snapshot of big for all of it.
			if _, err := db.Query(ctx, "SELECT (SELECT count(*) FROM big) + (SELECT count(*) FROM (WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM c WHERE x < 400000) SELECT x FROM c))"); err != nil {
				t.Error(err)
				return
			}
			reads.Add(1)
		}
	})
	var peak int64
	written := int64(0)
	for written < 96<<20 {
		_, err := db.Execute(ctx, "INSERT INTO big(b) VALUES (randomblob(65536))")
		require.NoError(t, err)
		written += 65536
		_, err = db.Execute(ctx, "DELETE FROM big WHERE id = (SELECT max(id) FROM big)")
		require.NoError(t, err)
		peak = max(peak, walSize(path))
	}
	close(stop)
	wg.Wait()
	t.Logf("WAL peak %.1f MB after %d MB written, %d reader loops", float64(peak)/1e6, written>>20, reads.Load())
	assert.Positive(t, reads.Load())
	assert.Less(t, peak, int64(64<<20), "WAL peak")
}

// TRUNCATE empties the WAL after an idle spell and at Close.
func TestTruncateAtIdleAndClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "godwit.db")
	db, err := Open(t.Context(), path)
	require.NoError(t, err)
	restartCheckpointer(db, 10*time.Millisecond, 100*time.Millisecond)
	_, err = db.Execute(t.Context(), "CREATE TABLE t(x BLOB)")
	require.NoError(t, err)
	_, err = db.Execute(t.Context(), "INSERT INTO t(x) VALUES (randomblob(100000))")
	require.NoError(t, err)
	_, err = db.Query(lane(t.Context(), Interactive), "SELECT count(*) FROM t") // readers open
	require.NoError(t, err)
	require.Eventually(t, func() bool { return walSize(path) == 0 }, 3*time.Second, 10*time.Millisecond, "TRUNCATE at idle")

	_, err = db.Execute(t.Context(), "INSERT INTO t(x) VALUES (randomblob(100000))")
	require.NoError(t, err)
	require.Positive(t, walSize(path))
	// A raw reader keeps the WAL file from being deleted at close, so the
	// size seen is the TRUNCATE's.
	other, err := sqlite.Open(t.Context(), path, sqlite.OpenOptions{ReadOnly: true})
	require.NoError(t, err)
	defer other.Close()
	require.NoError(t, db.Close())
	assert.Zero(t, walSize(path), "TRUNCATE at close")
	require.NoError(t, db.Close(), "a second Close is harmless")
}

// Close closes the readers before the writer, and a read after Close fails
// cleanly instead of hanging.
func TestCloseOrder(t *testing.T) {
	db, err := Open(t.Context(), filepath.Join(t.TempDir(), "godwit.db"))
	require.NoError(t, err)
	readersOf(t, db)
	require.NoError(t, db.Close())
	assert.Nil(t, db.lanes.readers.Load())
	_, err = db.Query(lane(t.Context(), Interactive), "SELECT 1")
	assert.True(t, errors.Is(err, sqlite.ErrClosed), "%v", err)
}
