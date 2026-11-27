package store

import (
	"context"
	"errors"
	"path/filepath"
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
	ui, bg := WithUIRead(t.Context()), t.Context()
	take := func(ctx context.Context) *reader {
		t.Helper()
		r, err := acquire(ctx, p)
		require.NoError(t, err)
		return r
	}
	give := func(r *reader) { r.lane <- struct{}{} }

	r := take(bg)
	assert.Same(t, p[2], r, "background reads use the shared reader")
	give(r)

	r0 := take(ui)
	assert.Same(t, p[0], r0)
	r1 := take(ui)
	assert.Same(t, p[1], r1, "a busy interface lane: the other one")
	shared := take(ui)
	assert.Same(t, p[2], shared, "both interface lanes busy: the free shared reader")

	short, cancel := context.WithTimeout(bg, 50*time.Millisecond)
	defer cancel()
	give(r0)
	_, err := acquire(short, p)
	assert.ErrorIs(t, err, context.DeadlineExceeded, "a background read never takes an interface lane")

	r0 = take(ui)
	got := make(chan *reader)
	go func() {
		r, _ := acquire(ui, p)
		got <- r
	}()
	select {
	case <-got:
		t.Fatal("an interface read took a busy reader")
	case <-time.After(50 * time.Millisecond):
	}
	give(r1)
	assert.Same(t, p[1], <-got, "waits for whichever reader frees first")
	give(p[1])
	give(r0)
	give(shared)
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
