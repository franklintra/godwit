package store

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/egoist/mygo/plugins/sqlite"
)

// Fork rewrites are the franklintra/godwit fork's only non-additive schema
// steps (FORK.md, "Fork rewrites"). A ForkMigration may only add; a rewrite
// may also replace triggers and move data, so it keeps three rules:
//
//   - Ensure runs on every Open, after the numbered migrations and the
//     ForkMigrations, in one transaction. It creates what is missing and
//     puts back each of its triggers whose SQL differs from its own (an
//     upstream migration may have redefined one). On a database that is up
//     to date it only reads sqlite_master.
//   - Backfill moves data in the background, one short transaction per
//     batch. Each batch leaves the database correct, so a crash or a stop
//     between two batches loses nothing and the next start goes on.
//   - The rewrite records its state in meta under "fork.<name>": "moving"
//     until Backfill finds nothing left, then "done".

// RewriteMoving and RewriteDone are the states a rewrite records in meta.
const (
	RewriteMoving = "moving"
	RewriteDone   = "done"
)

// Batch bounds one Backfill call: at most Rows rows and, past the first
// row, Bytes bytes of data.
type Batch struct {
	Rows  int
	Bytes int64
}

// BatchResult is what one Backfill call did. More is false once the
// rewrite is done.
type BatchResult struct {
	Rows  int
	Bytes int64
	More  bool
}

// ForkRewrite is one non-additive fork step.
type ForkRewrite struct {
	Name     string // unique; meta key "fork.<Name>"
	Ensure   func(ctx context.Context, db *DB) error
	Backfill func(ctx context.Context, db *DB, b Batch) (BatchResult, error)
	Undo     func(ctx context.Context, db *DB, progress func(moved int)) error // back to stock Godwit's layout (godwit fork-undo)
}

// ForkRewrites run after ForkMigrations, in order.
var ForkRewrites = []ForkRewrite{bodiesRewrite}

// ensureRewrites runs every rewrite's Ensure: the tail of applyFork.
func (db *DB) ensureRewrites(ctx context.Context) error {
	for _, r := range ForkRewrites {
		if err := r.Ensure(ctx, db); err != nil {
			return fmt.Errorf("store: fork rewrite %s: %w", r.Name, err)
		}
	}
	return nil
}

// RewriteState is rewrite name's state in meta: RewriteMoving, RewriteDone,
// or "" when it has none (a stock database, or one fork-undo restored).
func (db *DB) RewriteState(ctx context.Context, name string) (string, error) {
	rows, err := db.Query(ctx, "SELECT value FROM meta WHERE key = ?", "fork."+name)
	if err != nil || len(rows.Values) == 0 {
		return "", err
	}
	return Str(rows.Values[0][0]), nil
}

// Rewrite is the rewrite called name, if there is one.
func Rewrite(name string) (ForkRewrite, bool) {
	for _, r := range ForkRewrites {
		if r.Name == name {
			return r, true
		}
	}
	return ForkRewrite{}, false
}

// Path is the database's main file, as SQLite opened it.
func (db *DB) Path(ctx context.Context) (string, error) {
	rows, err := db.Query(ctx, "SELECT file FROM pragma_database_list WHERE name = 'main'")
	if err != nil {
		return "", err
	}
	if len(rows.Values) == 0 {
		return "", fmt.Errorf("store: no main database")
	}
	return Str(rows.Values[0][0]), nil
}

// forkTrigger is a trigger a rewrite owns: Ensure re-creates it when the
// database's differs.
type forkTrigger struct {
	name, sql string
}

var spaces = regexp.MustCompile(`\s+`)

// sameSQL compares two CREATE statements as SQLite keeps them in
// sqlite_master: the text as written, so only the spacing may differ.
func sameSQL(a, b string) bool {
	return spaces.ReplaceAllString(strings.TrimSpace(a), " ") == spaces.ReplaceAllString(strings.TrimSpace(b), " ")
}

// schemaSQL is sqlite_master's sql of each named object that exists.
func (db *DB) schemaSQL(ctx context.Context, names []string) (map[string]string, error) {
	q := "SELECT name, sql FROM sqlite_master WHERE name IN (" + strings.TrimSuffix(strings.Repeat("?,", len(names)), ",") + ")"
	args := make([]any, len(names))
	for i, n := range names {
		args[i] = n
	}
	rows, err := db.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, r := range rows.Values {
		out[Str(r[0])] = Str(r[1])
	}
	return out, nil
}

// triggerStmts are the statements that put back each trigger of want that
// is missing or differs from have.
func triggerStmts(want []forkTrigger, have map[string]string) []sqlite.Statement {
	var out []sqlite.Statement
	for _, t := range want {
		if s, ok := have[t.name]; ok && sameSQL(s, t.sql) {
			continue
		}
		out = append(out, sqlite.Statement{SQL: "DROP TRIGGER IF EXISTS " + t.name}, sqlite.Statement{SQL: t.sql})
	}
	return out
}

// setMeta is the statement that sets meta key to value.
func setMeta(key string, value any) sqlite.Statement {
	return sqlite.Statement{SQL: "INSERT INTO meta(key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value", Args: []any{key, value}}
}

// metaInt is meta key as an integer, 0 when absent.
func (db *DB) metaInt(ctx context.Context, key string) (int64, error) {
	rows, err := db.Query(ctx, "SELECT value FROM meta WHERE key = ?", key)
	if err != nil || len(rows.Values) == 0 {
		return 0, err
	}
	n, _ := strconv.ParseInt(Str(rows.Values[0][0]), 10, 64)
	return n, nil
}
