// Package store is Godwit's SQLite storage layer.
package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/egoist/mygo/plugins/sqlite"
	"github.com/godwit-email/godwit/internal/perftrace"
)

// ErrNewerDatabase is returned when the database was written by a newer Godwit.
var ErrNewerDatabase = errors.New("store: database schema is newer than this version")

// DB wraps a mygo sqlite connection, the writer, and read-only
// connections beside it that Query sends the interface's plain SELECTs to
// (readers.go).
type DB struct {
	*sqlite.DB
	rd      *readers // nil while Open migrates: every call on the writer
	syncMu  sync.Mutex
	syncing int
}

var isTest = testing.Testing

// Open opens (creating if needed) the database at path and applies pending migrations.
func Open(ctx context.Context, path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	raw, err := sqlite.Open(ctx, path, sqlite.OpenOptions{})
	perftrace.Mark("store.sqlite_open")
	if err != nil {
		return nil, err
	}
	db := &DB{DB: raw}
	if err := db.init(ctx, path); err != nil {
		raw.Close()
		return nil, err
	}
	// After init: the schema no longer changes and the WAL exists, which a
	// read-only connection needs to open.
	db.rd = &readers{path: path}
	db.rd.wal.checkpoint = func(ctx context.Context) error {
		_, err := db.Query(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
		return err
	}
	return db, nil
}

// Close closes the readers, then the writer.
func (db *DB) Close() error {
	var err error
	if db.rd != nil {
		err = db.rd.close()
	}
	return errors.Join(err, db.DB.Close())
}

// Query is sqlite.DB's, timed for perftrace's SQL trace when it is on. An
// interface read (WithUIRead) that is a plain SELECT (readOnlySQL) runs on
// a read-only connection; anything else on the writer.
func (db *DB) Query(ctx context.Context, sql string, args ...any) (sqlite.Rows, error) {
	if db.rd != nil && isUIRead(ctx) && readOnlySQL(sql) {
		if pool := db.rd.get(); pool != nil && !db.rd.wal.draining.Load() {
			if rows, err := db.rd.read(ctx, pool, sql, args); err != errDraining {
				return rows, err
			}
		}
	}
	if !perftrace.SQLOn() {
		return db.DB.Query(ctx, sql, args...)
	}
	t := time.Now()
	r, err := db.DB.Query(ctx, sql, args...)
	perftrace.SQL(ctx, "q", sql, t)
	return r, err
}

// Execute is sqlite.DB's, timed as Query is. While the readers hold the
// WAL past walLimit, the backend's writes wait (readers_wal.go).
func (db *DB) Execute(ctx context.Context, sql string, args ...any) (sqlite.Result, error) {
	if db.rd != nil {
		db.rd.beforeWrite(ctx)
	}
	if !perftrace.SQLOn() {
		return db.DB.Execute(ctx, sql, args...)
	}
	t := time.Now()
	r, err := db.DB.Execute(ctx, sql, args...)
	perftrace.SQL(ctx, "x", sql, t)
	return r, err
}

// Transaction is sqlite.DB's, timed as Query is (by its first statement),
// and waits as Execute does.
func (db *DB) Transaction(ctx context.Context, stmts []sqlite.Statement) ([]sqlite.Result, error) {
	if db.rd != nil {
		db.rd.beforeWrite(ctx)
	}
	if !perftrace.SQLOn() {
		return db.DB.Transaction(ctx, stmts)
	}
	t := time.Now()
	r, err := db.DB.Transaction(ctx, stmts)
	first := ""
	if len(stmts) > 0 {
		first = stmts[0].SQL
	}
	perftrace.SQL(ctx, fmt.Sprintf("t%d", len(stmts)), first, t)
	return r, err
}

// DurableTransaction runs stmts in a Transaction with synchronous = FULL
// (outside tests), syncing the WAL to disk on commit so an OS crash or power
// loss cannot roll back a committed claim before an external effect (ADR 0006,
// ADR 0004) or a row deletion before removing its files or keychain entries.
func (db *DB) DurableTransaction(ctx context.Context, stmts []sqlite.Statement) ([]sqlite.Result, error) {
	done, err := db.fullSync(ctx)
	if err != nil {
		return nil, err
	}
	defer done()
	return db.Transaction(ctx, stmts)
}

// DurableExecute is Execute with synchronous = FULL (outside tests), as
// DurableTransaction is for Transaction.
func (db *DB) DurableExecute(ctx context.Context, sql string, args ...any) (sqlite.Result, error) {
	done, err := db.fullSync(ctx)
	if err != nil {
		return sqlite.Result{}, err
	}
	defer done()
	return db.Execute(ctx, sql, args...)
}

func (db *DB) fullSync(ctx context.Context) (func(), error) {
	if isTest() {
		return func() {}, nil
	}
	db.syncMu.Lock()
	if db.syncing == 0 {
		if _, err := db.Query(ctx, "PRAGMA synchronous = FULL"); err != nil {
			db.syncMu.Unlock()
			return nil, err
		}
	}
	db.syncing++
	db.syncMu.Unlock()
	return func() {
		db.syncMu.Lock()
		defer db.syncMu.Unlock()
		if db.syncing == 1 {
			if _, err := db.Query(context.Background(), "PRAGMA synchronous = NORMAL"); err != nil {
				return
			}
		}
		db.syncing--
	}, nil
}

func (db *DB) init(ctx context.Context, path string) error {
	rows, err := db.Query(ctx, "PRAGMA user_version")
	if err != nil {
		return err
	}
	cur := int(Int(rows.Values[0][0]))
	if cur > len(Migrations) {
		return fmt.Errorf("%w (have %d, know %d)", ErrNewerDatabase, cur, len(Migrations))
	}
	// Tests open a new database per case; OFF skips every fsync. Under WAL,
	// NORMAL is crash-safe and syncs at checkpoints instead of every commit.
	synchronous := "PRAGMA synchronous = NORMAL"
	if isTest() {
		synchronous = "PRAGMA synchronous = OFF"
	}
	// Only a new database takes auto_vacuum as set, and only before WAL
	// writes its header; an older one converts with VACUUM below.
	pragmas := []string{synchronous, "PRAGMA auto_vacuum = INCREMENTAL", "PRAGMA foreign_keys = ON", "PRAGMA journal_mode = WAL"}
	for _, p := range pragmas {
		if _, err := db.Query(ctx, p); err != nil {
			return err
		}
	}
	perftrace.Mark("store.pragmas")
	if cur > 0 && cur < PiMessagesVersion {
		backup(ctx, db, path, cur)
	}
	for v := cur + 1; v <= len(Migrations); v++ {
		todo, err := db.skipForkObjects(ctx, v, Migrations[v-1])
		if err != nil {
			return err
		}
		var stmts []sqlite.Statement
		for _, s := range todo {
			stmts = append(stmts, sqlite.Statement{SQL: s})
		}
		stmts = append(stmts, sqlite.Statement{SQL: fmt.Sprintf("PRAGMA user_version = %d", v)})
		if _, err := db.Transaction(ctx, stmts); err != nil {
			return fmt.Errorf("store: migration %d: %w", v, err)
		}
	}
	perftrace.Mark("store.migrations")
	if err := db.applyFork(ctx); err != nil {
		return err
	}
	perftrace.Mark("store.fork")
	dropBackups(path, cur)
	err = db.convertAutoVacuum(ctx)
	perftrace.Mark("store.auto_vacuum")
	return err
}

// backup copies the database at path, at schema version cur, to
// path.bak-v<cur> before a migration older versions cannot open; it is
// best effort (no room: no backup) and dropped a version later (dropBackups).
func backup(ctx context.Context, db *DB, path string, cur int) {
	// The copy is the main file alone: the WAL is folded in and emptied first.
	if _, err := db.Query(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		slog.Default().Warn("could not back the database up", "err", err)
		return
	}
	dst := fmt.Sprintf("%s.bak-v%d", path, cur)
	if _, err := os.Stat(dst); err == nil {
		return // an earlier attempt's: kept as it is
	}
	if err := copyFile(path, dst); err != nil {
		os.Remove(dst) // what this attempt wrote of it
		slog.Default().Warn("could not back the database up", "err", err)
	}
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// dropBackups removes path.bak-v<n> backups once a start begins past n+1
// (cur, the version before migrating): a version later than the migration
// they were taken for has run. Not by the version just migrated to: one
// start that migrates past n+1 would drop the backup it just took.
func dropBackups(path string, cur int) {
	olds, _ := filepath.Glob(path + ".bak-v*")
	for _, f := range olds {
		var n int
		if _, err := fmt.Sscanf(strings.TrimPrefix(f, path+".bak-v"), "%d", &n); err == nil && cur > n+1 {
			os.Remove(f)
		}
	}
}

// convertAutoVacuum turns incremental auto_vacuum on for a database made
// before it was set: a VACUUM, which needs up to twice the file's size free;
// one that fails (no room, say) leaves the database as it was until the
// next open tries again.
func (db *DB) convertAutoVacuum(ctx context.Context) error {
	rows, err := db.Query(ctx, "PRAGMA auto_vacuum")
	if err != nil {
		return err
	}
	if Int(rows.Values[0][0]) == autoVacuumIncremental {
		return nil
	}
	if _, err := db.Query(ctx, "PRAGMA auto_vacuum = INCREMENTAL"); err != nil {
		return err
	}
	if _, err := db.Execute(ctx, "VACUUM"); err != nil {
		slog.Default().Warn("could not convert the database to incremental vacuum", "err", err)
	}
	return nil
}

const autoVacuumIncremental = 2

// Reclaim thresholds, as Codex's: free pages are given back once they are at
// least reclaimMin and reclaimShare of the file, down to what keeps
// reclaimKeep (or reclaimKeepShare) free for reuse, reclaimBatch pages a time.
const (
	reclaimMin       = 64 << 20
	reclaimShare     = 0.25
	reclaimKeep      = 16 << 20
	reclaimKeepShare = 0.10
	reclaimBatch     = 64
)

// Reclaim gives free pages back to the file system, a batch at a time, when
// enough of the file is free; it stops at ctx's end.
func (db *DB) Reclaim(ctx context.Context) error {
	rows, err := db.Query(ctx, "SELECT (SELECT page_size FROM pragma_page_size), (SELECT freelist_count FROM pragma_freelist_count), (SELECT page_count FROM pragma_page_count)")
	if err != nil {
		return err
	}
	size, free, total := Int(rows.Values[0][0]), Int(rows.Values[0][1]), Int(rows.Values[0][2])
	if size == 0 || total == 0 || free*size < reclaimMin || float64(free)/float64(total) < reclaimShare {
		return nil
	}
	keep := max(reclaimKeep/size, int64(float64(total)*reclaimKeepShare))
	for n := free - keep; n > 0 && ctx.Err() == nil; n -= reclaimBatch {
		if _, err := db.Query(ctx, fmt.Sprintf("PRAGMA incremental_vacuum(%d)", min(n, reclaimBatch))); err != nil {
			return err
		}
	}
	_, err = db.Query(ctx, "PRAGMA wal_checkpoint(PASSIVE)")
	return err
}
