package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/egoist/mygo/plugins/sqlite"
)

// Querier runs one read statement; *DB is one.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (sqlite.Rows, error)
}

// ErrNoProgress is Chunks' error when a chunk's last key does not pass the
// key it started after: the scan would loop.
var ErrNoProgress = errors.New("store: keyset scan made no progress")

// Chunks runs a keyset scan as one statement per chunk (ADR 0052).
//
// mygo's Query runs a statement to its end and copies every row, so one
// statement is one read snapshot for as long as it runs, and a long one on
// a reader keeps the WAL from restarting. A scan over a big table instead
// runs chunks of at most size rows: chunk(after, size) returns a statement
// of the form "... WHERE <key> > ? ... ORDER BY <key> LIMIT ?" with its
// arguments, each gets the chunk's rows and returns the largest key in
// them. The scan starts after the key after (0, or math.MinInt64 for keys
// that can be negative) and ends at a chunk of fewer than size rows, at
// ctx's end, or at each's error.
//
// Each chunk is its own snapshot: rows written between two chunks show in
// the later one if their key is past the cursor, and a row moved below the
// cursor is not seen again. A caller must accept that, as it already
// accepts changes between its statements.
func Chunks(ctx context.Context, db Querier, after int64, size int, chunk func(after int64, limit int) (string, []any), each func(sqlite.Rows) (last int64, err error)) error {
	if size <= 0 {
		return fmt.Errorf("store: chunk size %d", size)
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		sql, args := chunk(after, size)
		rows, err := db.Query(ctx, sql, args...)
		if err != nil {
			return err
		}
		if len(rows.Values) == 0 {
			return nil
		}
		last, err := each(rows)
		if err != nil {
			return err
		}
		if len(rows.Values) < size {
			return nil
		}
		if last <= after {
			return ErrNoProgress
		}
		after = last
	}
}
