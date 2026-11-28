package store

import (
	"context"
	"testing"

	"github.com/egoist/mygo/plugins/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func keyset(after int64, limit int) (string, []any) {
	return "SELECT rowid, x FROM t WHERE rowid > ? ORDER BY rowid LIMIT ?", []any{after, limit}
}

func lastKey(seen *[]int64) func(sqlite.Rows) (int64, error) {
	return func(rows sqlite.Rows) (int64, error) {
		var last int64
		for _, r := range rows.Values {
			last = Int(r[0])
			*seen = append(*seen, Int(r[1]))
		}
		return last, nil
	}
}

// A keyset scan in chunks sees every row once, in order, for any chunk size,
// and equals the single statement it replaces.
func TestChunksEqualOneStatement(t *testing.T) {
	db := openLanesDB(t)
	ctx := t.Context()
	_, err := db.Execute(ctx, "INSERT INTO t(x) SELECT x FROM (WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM c WHERE x < 1000) SELECT x FROM c)")
	require.NoError(t, err)
	_, err = db.Execute(ctx, "DELETE FROM t WHERE x % 7 = 0") // holes in the keys
	require.NoError(t, err)
	rows, err := db.Query(ctx, "SELECT x FROM t ORDER BY rowid")
	require.NoError(t, err)
	var want []int64
	for _, r := range rows.Values {
		want = append(want, Int(r[0]))
	}
	for _, size := range []int{1, 7, 64, 857, 1000, 5000} {
		var seen []int64
		calls := 0
		err := Chunks(ctx, db, 0, size, func(after int64, limit int) (string, []any) {
			calls++
			return keyset(after, limit)
		}, lastKey(&seen))
		require.NoError(t, err, size)
		assert.Equal(t, want, seen, "size %d", size)
		assert.LessOrEqual(t, calls, len(want)/size+2, "size %d", size)
	}
}

func TestChunksEdges(t *testing.T) {
	db := openLanesDB(t)
	ctx := t.Context()
	var seen []int64
	require.NoError(t, Chunks(ctx, db, 0, 10, keyset, lastKey(&seen)), "an empty table")
	assert.Empty(t, seen)

	_, err := db.Execute(ctx, "INSERT INTO t(x) SELECT x FROM (WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM c WHERE x < 30) SELECT x FROM c)")
	require.NoError(t, err)
	err = Chunks(ctx, db, 0, 10, keyset, func(sqlite.Rows) (int64, error) { return 0, nil })
	assert.ErrorIs(t, err, ErrNoProgress, "a cursor that does not move")

	gone, cancel := context.WithCancel(ctx)
	n := 0
	err = Chunks(gone, db, 0, 10, keyset, func(rows sqlite.Rows) (int64, error) {
		n++
		cancel()
		return Int(rows.Values[len(rows.Values)-1][0]), nil
	})
	assert.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, n, "the scan stops at ctx's end")

	assert.Error(t, Chunks(ctx, db, 0, 0, keyset, lastKey(&seen)))
}
