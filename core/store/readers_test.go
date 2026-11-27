package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/egoist/mygo/plugins/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func openReadersDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(t.Context(), filepath.Join(t.TempDir(), "godwit.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	_, err = db.Execute(t.Context(), "CREATE TABLE IF NOT EXISTS t(x INTEGER)")
	require.NoError(t, err)
	return db
}

func pool(t *testing.T, db *DB) []*reader {
	t.Helper()
	p := db.rd.get()
	require.Len(t, p, readerCount)
	return p
}

func TestReadOnlySQL(t *testing.T) {
	for sql, want := range map[string]bool{
		"SELECT 1": true,
		"  \n\tselect id FROM messages WHERE id = ?":      true,
		"-- note\nSELECT 1":                               true,
		"/* a */ /* b */SELECT 1":                         true,
		"SELECT (SELECT page_size FROM pragma_page_size)": true,
		"SELECT changes_at FROM t":                        true, // a column, not the function
		"SELECT x FROM t WHERE label = 'changes'":         true,
		"PRAGMA user_version":                             false,
		"PRAGMA wal_checkpoint(PASSIVE)":                  false,
		"WITH c AS (SELECT 1) SELECT * FROM c":            false,
		"WITH c AS (SELECT 1) DELETE FROM t":              false,
		"INSERT INTO t(x) VALUES (1) RETURNING x":         false,
		"UPDATE t SET x = 1 RETURNING x":                  false,
		"DELETE FROM t RETURNING x":                       false,
		"SELECT last_insert_rowid()":                      false,
		"SELECT changes()":                                false,
		"SELECT total_changes ()":                         false,
		"select CHANGES ( )":                              false,
		"SELECTx":                                         false,
		"VALUES (1)":                                      false,
		"-- only a comment":                               false,
		"/* unterminated SELECT 1":                        false,
		"":                                                false,
	} {
		assert.Equal(t, want, readOnlySQL(sql), sql)
	}
}

func TestReadersOpenOnFirstRead(t *testing.T) {
	db := openReadersDB(t)
	ui := WithUIRead(t.Context())
	_, err := db.Query(ui, "PRAGMA user_version")
	require.NoError(t, err)
	assert.Nil(t, db.rd.pool.Load(), "a PRAGMA stays on the writer")
	_, err = db.Query(t.Context(), "SELECT count(*) FROM t")
	require.NoError(t, err)
	assert.Nil(t, db.rd.pool.Load(), "a background read stays on the writer")
	_, err = db.Query(ui, "SELECT count(*) FROM t")
	require.NoError(t, err)
	assert.NotNil(t, db.rd.pool.Load())
}

func TestReaderCacheSize(t *testing.T) {
	db := openReadersDB(t)
	for _, r := range pool(t, db) {
		rows, err := r.db.Query(t.Context(), "PRAGMA cache_size")
		require.NoError(t, err)
		assert.EqualValues(t, -readerCacheKiB, Int(rows.Values[0][0]), r.kind)
	}
}

func TestNoReadersUntilARead(t *testing.T) {
	db := openReadersDB(t)
	_, err := db.Transaction(t.Context(), []sqlite.Statement{{SQL: "INSERT INTO t(x) VALUES (1)"}})
	require.NoError(t, err)
	rows, err := db.DB.Query(t.Context(), "PRAGMA journal_mode = DELETE")
	require.NoError(t, err)
	assert.Equal(t, "delete", Str(rows.Values[0][0]))
}

func TestReadSeesCommittedWrite(t *testing.T) {
	db := openReadersDB(t)
	pool(t, db)
	for i := range 20 {
		done := make(chan error)
		go func() {
			_, err := db.Execute(context.Background(), "INSERT INTO t(x) VALUES (?)", i)
			done <- err
		}()
		require.NoError(t, <-done)
		for _, ctx := range []context.Context{t.Context(), WithUIRead(t.Context()), WithLongRead(WithUIRead(t.Context()))} {
			rows, err := db.Query(ctx, "SELECT count(*) FROM t")
			require.NoError(t, err)
			assert.EqualValues(t, i+1, Int(rows.Values[0][0]))
		}
	}
}

func TestWriteOnAReaderFails(t *testing.T) {
	db := openReadersDB(t)
	p := pool(t, db)
	_, err := db.rd.read(t.Context(), p, "INSERT INTO t(x) VALUES (1) RETURNING x", nil)
	var se *sqlite.Error
	require.ErrorAs(t, err, &se)
	assert.Equal(t, 8, se.Code, "SQLITE_READONLY")
	rows, err := db.Query(t.Context(), "SELECT count(*) FROM t")
	require.NoError(t, err)
	assert.EqualValues(t, 0, Int(rows.Values[0][0]))
}

func TestAcquireLanes(t *testing.T) {
	db := openReadersDB(t)
	p := pool(t, db)
	ctx := t.Context()
	take := func(c readClass) *reader {
		t.Helper()
		r, err := acquire(ctx, p, c, nil)
		require.NoError(t, err)
		return r
	}
	give := func(r *reader) { r.lane <- struct{}{} }
	busy := func(c readClass) {
		t.Helper()
		short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		defer cancel()
		_, err := acquire(short, p, c, nil)
		assert.ErrorIs(t, err, context.DeadlineExceeded)
	}

	r := take(quickRead)
	assert.Same(t, p[0], r, "a quick read takes the quick lane first")
	give(r)
	r = take(longRead)
	assert.Same(t, p[1], r, "a long read takes the long lane")
	give(r)

	q1, q2 := take(quickRead), take(quickRead)
	assert.Same(t, p[0], q1)
	assert.Same(t, p[1], q2, "the quick lane busy: the free long lane")
	busy(quickRead)
	give(q2)

	l := take(longRead)
	assert.Same(t, p[1], l, "a long read does not need the quick lane")
	give(q1)
	busy(longRead) // a long read never takes the quick lane
	q := take(quickRead)
	assert.Same(t, p[0], q, "a long read leaves the quick lane free")

	got := make(chan *reader)
	go func() {
		r, _ := acquire(ctx, p, quickRead, nil)
		got <- r
	}()
	select {
	case <-got:
		t.Fatal("a read took a busy reader")
	case <-time.After(50 * time.Millisecond):
	}
	give(l)
	assert.Same(t, p[1], <-got, "waits for whichever reader frees first")
	give(p[1])
	give(q)

	f := take(foregroundScan)
	assert.Same(t, p[1], f, "a foreground scan takes the long lane first")
	f2 := take(foregroundScan)
	assert.Same(t, p[0], f2, "the long lane busy: the free quick lane, not a queue")
	busy(foregroundScan)
	give(f2)
	give(f)

	l = take(longRead)
	queued := make(chan *reader)
	go func() {
		r, _ := acquire(ctx, p, longRead, nil)
		queued <- r
	}()
	time.Sleep(20 * time.Millisecond) // a long read queues for the long lane
	f = take(foregroundScan)
	assert.Same(t, p[0], f, "a foreground scan does not queue behind the long lane's scans")
	give(f)
	give(l)
	give(<-queued)
}

func TestReadClass(t *testing.T) {
	db := openReadersDB(t)
	rs := db.rd
	ui := WithUIRead(t.Context())
	assert.Equal(t, quickRead, rs.class(ui, "SELECT 1"), "a statement not run yet is quick")
	assert.Equal(t, longRead, rs.class(WithLongRead(ui), "SELECT 1"), "unless marked")
	rs.ran("SELECT 1", time.Millisecond)
	assert.Equal(t, quickRead, rs.class(ui, "SELECT 1"))
	assert.Equal(t, longRead, rs.class(WithLongRead(ui), "SELECT 1"), "a mark wins over a quick run")
	rs.ran("SELECT 1", slowRead)
	assert.Equal(t, longRead, rs.class(ui, "SELECT 1"))
	assert.Equal(t, foregroundScan, rs.class(WithForegroundScan(ui), "SELECT 1"), "a foreground scan stays one however long it ran")
	assert.Equal(t, longRead, rs.class(WithLongRead(WithForegroundScan(ui)), "SELECT 1"), "the long mark wins")
	for i := range maxTimed {
		rs.ran("SELECT "+strconv.Itoa(i+2), time.Millisecond)
	}
	assert.LessOrEqual(t, len(rs.timed), maxTimed, "the table stays bounded")

	_, err := db.Query(ui, "SELECT count(*) FROM t")
	require.NoError(t, err)
	_, recorded := rs.timed["SELECT count(*) FROM t"]
	assert.True(t, recorded, "a read records its run time")
	assert.Equal(t, quickRead, rs.class(ui, "SELECT count(*) FROM t"))
}

const endless = "SELECT count(*) FROM (WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM c) SELECT x FROM c)"

// A long background read holds the writer, as before the readers; an
// interface read neither waits for it nor takes a reader from it, and
// canceling it interrupts the statement.
func TestLongBackgroundReadBlocksNoInterfaceRead(t *testing.T) {
	db := openReadersDB(t)
	p := pool(t, db)
	_, err := db.Execute(t.Context(), "INSERT INTO t(x) VALUES (1)")
	require.NoError(t, err)
	long, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	started := time.Now()
	go func() {
		_, err := db.Query(long, endless)
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	assert.Len(t, p[0].lane, 1, "the background read took no reader")
	assert.Len(t, p[1].lane, 1, "the background read took no reader")

	quick, stop := context.WithTimeout(WithUIRead(t.Context()), 2*time.Second)
	defer stop()
	rows, err := db.Query(quick, "SELECT count(*) FROM t")
	require.NoError(t, err, "an interface read does not wait for the writer")
	assert.EqualValues(t, 1, Int(rows.Values[0][0]))
	rows, err = db.Query(WithLongRead(quick), "SELECT count(*) FROM t")
	require.NoError(t, err, "nor does a long one")
	assert.EqualValues(t, 1, Int(rows.Values[0][0]))

	queued, stop2 := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer stop2()
	_, err = db.Query(queued, "SELECT 1")
	assert.ErrorIs(t, err, context.DeadlineExceeded, "another background read queues on the writer")

	select {
	case err := <-done:
		t.Fatalf("the long read ended by itself after %s: %v", time.Since(started), err)
	default:
	}
	start := time.Now()
	cancel()
	select {
	case err := <-done:
		assert.ErrorIs(t, err, context.Canceled)
		assert.Less(t, time.Since(start), 2*time.Second)
	case <-time.After(5 * time.Second):
		t.Fatal("canceling did not interrupt the writer's statement")
	}
}

func TestCloseClosesReaders(t *testing.T) {
	db := openReadersDB(t)
	p := pool(t, db)
	require.NoError(t, db.Close())
	for _, r := range p {
		_, err := r.db.Query(context.Background(), "SELECT 1")
		assert.ErrorIs(t, err, sqlite.ErrClosed)
	}
	_, err := db.Query(context.Background(), "SELECT 1")
	assert.ErrorIs(t, err, sqlite.ErrClosed, "after Close a read reaches the closed writer")
	assert.Nil(t, db.rd.get(), "no reader opens after Close")
	assert.NoError(t, db.Close(), "closing again is harmless")
}

func TestReadersFailToOpenFallsBackToWriter(t *testing.T) {
	rs := &readers{path: filepath.Join(t.TempDir(), "missing", "godwit.db")}
	assert.Nil(t, rs.get())
	assert.True(t, rs.off)
	assert.NoError(t, errors.Join(rs.close()))
}

// A long interface read holds the long lane; a quick interface read
// still runs at once on the quick lane, a second long one waits, and
// canceling interrupts the reader's statement.
func TestLongInterfaceReadLeavesTheQuickLane(t *testing.T) {
	db := openReadersDB(t)
	p := pool(t, db)
	ui, cancel := context.WithCancel(WithUIRead(t.Context()))
	done := make(chan error, 1)
	go func() {
		_, err := db.Query(WithLongRead(ui), endless)
		done <- err
	}()
	require.Eventually(t, func() bool { return len(p[1].lane) == 0 }, 5*time.Second, time.Millisecond)

	quick, stop := context.WithTimeout(WithUIRead(t.Context()), 2*time.Second)
	defer stop()
	_, err := db.Query(quick, "SELECT count(*) FROM t")
	require.NoError(t, err, "a quick read does not wait for a long one")
	assert.Len(t, p[0].lane, 1, "it ran on the quick lane and gave it back")

	queued, stop2 := context.WithTimeout(WithLongRead(WithUIRead(t.Context())), 50*time.Millisecond)
	defer stop2()
	_, err = db.Query(queued, "SELECT count(*) FROM t")
	assert.ErrorIs(t, err, context.DeadlineExceeded, "a second long read waits for the long lane")

	_, err = db.Execute(t.Context(), "INSERT INTO t(x) VALUES (1)")
	require.NoError(t, err, "the writer does not wait for a reader")

	start := time.Now()
	cancel()
	select {
	case err := <-done:
		assert.ErrorIs(t, err, context.Canceled)
		assert.Less(t, time.Since(start), 2*time.Second)
	case <-time.After(5 * time.Second):
		t.Fatal("canceling did not interrupt the reader's statement")
	}
	assert.Len(t, p[1].lane, 1, "the lane is free again")
}

// The interface's mark lives as long as the context that set it: work
// detached with context.WithoutCancel reads on a reader while the
// connection lives and on the writer after; WithoutUIRead drops it.
func TestUIReadLasts(t *testing.T) {
	assert.False(t, isUIRead(t.Context()))
	assert.True(t, isUIRead(WithUIRead(context.Background())), "a context that never ends")
	conn, end := context.WithCancel(t.Context())
	ui := WithUIRead(conn)
	call, done := context.WithCancel(ui)
	detached := context.WithoutCancel(call)
	done()
	assert.True(t, isUIRead(detached), "the call ended, the connection lives")
	assert.False(t, isUIRead(WithoutUIRead(detached)))
	assert.False(t, isUIRead(WithoutUIRead(t.Context())))
	end()
	assert.False(t, isUIRead(detached), "the connection ended")
	assert.False(t, isUIRead(ui))
}

// Past the WAL limit, with a reader holding a snapshot, the backend's
// writes wait while the readers drain; the interface still reads (on the
// writer, a read already waiting for a lane included) and writes; once the reader's statement ends, the WAL is
// truncated and the writes resume.
func TestWALDrainPausesBackgroundWrites(t *testing.T) {
	db := openReadersDB(t)
	p := pool(t, db)
	db.rd.wal.limit = 1 // any WAL at all
	_, err := db.Execute(t.Context(), "INSERT INTO t(x) VALUES (1)")
	require.NoError(t, err, "a write made while no reader runs")

	ui := WithUIRead(t.Context())
	long, cancel := context.WithCancel(WithLongRead(ui))
	held := make(chan error, 1)
	go func() {
		_, err := db.Query(long, endless)
		held <- err
	}()
	require.Eventually(t, func() bool { return len(p[1].lane) == 0 }, 5*time.Second, time.Millisecond)
	queued := make(chan error, 1)
	go func() {
		_, err := db.Query(WithLongRead(ui), "SELECT count(*) FROM t")
		queued <- err
	}()
	time.Sleep(20 * time.Millisecond) // it waits for the long lane

	wrote := make(chan error, 1)
	go func() {
		db.rd.wal.mu.Lock()
		db.rd.wal.next = time.Time{}
		db.rd.wal.mu.Unlock()
		_, err := db.Execute(context.Background(), "INSERT INTO t(x) VALUES (2)")
		wrote <- err
	}()
	require.Eventually(t, db.rd.wal.draining.Load, 5*time.Second, time.Millisecond)
	select {
	case err := <-queued:
		require.NoError(t, err, "a read waiting for a lane when the drain starts reads on the writer")
	case <-time.After(2 * time.Second):
		t.Fatal("a read waiting for a lane still waits for it during the drain")
	}
	select {
	case <-wrote:
		t.Fatal("a background write did not wait for the drain")
	case <-time.After(100 * time.Millisecond):
	}

	quick, stop := context.WithTimeout(ui, 2*time.Second)
	defer stop()
	rows, err := db.Query(quick, "SELECT count(*) FROM t")
	require.NoError(t, err, "an interface read takes the writer during a drain")
	assert.EqualValues(t, 1, Int(rows.Values[0][0]))
	_, err = db.Execute(quick, "INSERT INTO t(x) VALUES (3)")
	require.NoError(t, err, "the interface's writes do not wait")

	cancel()
	assert.ErrorIs(t, <-held, context.Canceled)
	select {
	case err := <-wrote:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("the background write still waits after the readers emptied")
	}
	assert.False(t, db.rd.wal.draining.Load())
	assert.Len(t, p[0].lane, 1)
	assert.Len(t, p[1].lane, 1, "the drain gave the lanes back")
	assert.Less(t, fileSize(t, db.rd.path+"-wal"), int64(64<<10), "the WAL was truncated before the write")
	rows, err = db.Query(ui, "SELECT count(*) FROM t")
	require.NoError(t, err)
	assert.EqualValues(t, 3, Int(rows.Values[0][0]))
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	require.NoError(t, err)
	return fi.Size()
}
