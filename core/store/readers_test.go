package store

import (
	"context"
	"errors"
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
	_, err := db.Query(t.Context(), "PRAGMA user_version")
	require.NoError(t, err)
	assert.Nil(t, db.rd.pool.Load(), "a PRAGMA stays on the writer")
	_, err = db.Query(t.Context(), "SELECT count(*) FROM t")
	require.NoError(t, err)
	assert.NotNil(t, db.rd.pool.Load())
}

// The dev database copy (core/devsync.go) migrates, writes, then switches
// the writer to journal_mode = DELETE, which needs no other connection.
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
		for _, ctx := range []context.Context{t.Context(), WithUIRead(t.Context())} {
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
		r, err := acquire(ctx, p, c)
		require.NoError(t, err)
		return r
	}
	give := func(r *reader) { r.lane <- struct{}{} }
	busy := func(c readClass) {
		t.Helper()
		short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		defer cancel()
		_, err := acquire(short, p, c)
		assert.ErrorIs(t, err, context.DeadlineExceeded)
	}

	r := take(backgroundRead)
	assert.Same(t, p[2], r, "background reads use the shared reader")
	give(r)
	r = take(quickRead)
	assert.Same(t, p[0], r, "a quick read takes the quick lane first")
	give(r)
	r = take(newRead)
	assert.Same(t, p[1], r, "a new read leaves the quick lane to quick ones")
	give(r)

	l1, l2 := take(longRead), take(longRead)
	assert.Same(t, p[1], l1)
	assert.Same(t, p[2], l2, "both long lanes taken: the free shared reader")
	busy(longRead) // a long read never takes the quick lane
	busy(backgroundRead)
	q := take(quickRead)
	assert.Same(t, p[0], q, "long reads leave the quick lane free")
	busy(quickRead)
	busy(newRead)
	give(q)
	n := take(newRead)
	assert.Same(t, p[0], n, "a new read takes the quick lane when it is the only free one")

	got := make(chan *reader)
	go func() {
		r, _ := acquire(ctx, p, quickRead)
		got <- r
	}()
	select {
	case <-got:
		t.Fatal("a read took a busy reader")
	case <-time.After(50 * time.Millisecond):
	}
	give(l2)
	assert.Same(t, p[2], <-got, "waits for whichever reader frees first")
	give(p[2])
	give(l1)
	give(n)
}

func TestReadClass(t *testing.T) {
	db := openReadersDB(t)
	rs := db.rd
	ui := WithUIRead(t.Context())
	assert.Equal(t, backgroundRead, rs.class(t.Context(), "SELECT 1"))
	assert.Equal(t, newRead, rs.class(ui, "SELECT 1"))
	rs.ran("SELECT 1", time.Millisecond)
	assert.Equal(t, quickRead, rs.class(ui, "SELECT 1"))
	rs.ran("SELECT 1", slowRead)
	assert.Equal(t, longRead, rs.class(ui, "SELECT 1"))
	assert.Equal(t, backgroundRead, rs.class(t.Context(), "SELECT 1"), "a background read stays on its lane")
	for i := range maxTimed {
		rs.ran("SELECT "+strconv.Itoa(i+2), time.Millisecond)
	}
	assert.LessOrEqual(t, len(rs.timed), maxTimed, "the table stays bounded")

	_, err := db.Query(ui, "SELECT count(*) FROM t")
	require.NoError(t, err)
	assert.Equal(t, quickRead, rs.class(ui, "SELECT count(*) FROM t"), "a read records its run time")
}

// A long background read holds the shared reader; neither an interface
// read nor a write waits for it, and canceling it interrupts the statement.
func TestLongBackgroundReadBlocksNoOne(t *testing.T) {
	db := openReadersDB(t)
	p := pool(t, db)
	long, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := db.Query(long, "SELECT count(*) FROM (WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM c) SELECT x FROM c)")
		done <- err
	}()
	require.Eventually(t, func() bool { return len(p[2].lane) == 0 }, 5*time.Second, time.Millisecond, "the long read holds the shared reader")

	quick, stop := context.WithTimeout(t.Context(), 2*time.Second)
	defer stop()
	_, err := db.Execute(quick, "INSERT INTO t(x) VALUES (1)")
	require.NoError(t, err, "the writer does not wait for a reader")
	rows, err := db.Query(WithUIRead(quick), "SELECT count(*) FROM t")
	require.NoError(t, err, "an interface read does not wait for a background read")
	assert.EqualValues(t, 1, Int(rows.Values[0][0]))

	queued, stop2 := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer stop2()
	_, err = db.Query(queued, "SELECT 1")
	assert.ErrorIs(t, err, context.DeadlineExceeded, "a background read queues behind the shared reader")

	select {
	case err := <-done:
		t.Fatalf("the long read ended by itself: %v", err)
	default:
	}
	start := time.Now()
	cancel()
	select {
	case err := <-done:
		assert.ErrorIs(t, err, context.Canceled)
		assert.Less(t, time.Since(start), 2*time.Second)
	case <-time.After(5 * time.Second):
		t.Fatal("canceling did not interrupt the reader's statement")
	}
	assert.Len(t, p[2].lane, 1, "the lane is free again")
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

// Two long interface reads hold both lanes they may take; a quick
// interface read still runs at once on the quick lane.
func TestLongInterfaceReadsLeaveTheQuickLane(t *testing.T) {
	db := openReadersDB(t)
	p := pool(t, db)
	const long = "SELECT count(*) FROM (WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM c) SELECT x FROM c)"
	db.rd.ran(long, slowRead)
	db.rd.ran("SELECT count(*) FROM t", time.Millisecond)
	ui, cancel := context.WithCancel(WithUIRead(t.Context()))
	done := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := db.Query(ui, long)
			done <- err
		}()
	}
	require.Eventually(t, func() bool { return len(p[1].lane) == 0 && len(p[2].lane) == 0 }, 5*time.Second, time.Millisecond)

	quick, stop := context.WithTimeout(WithUIRead(t.Context()), 2*time.Second)
	defer stop()
	_, err := db.Query(quick, "SELECT count(*) FROM t")
	require.NoError(t, err, "a quick read does not wait for long ones")
	assert.Len(t, p[0].lane, 1, "it ran on the quick lane and gave it back")

	cancel()
	for range 2 {
		assert.ErrorIs(t, <-done, context.Canceled)
	}
}
