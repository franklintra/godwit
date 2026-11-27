package store_test

import (
	"path/filepath"
	"testing"

	"github.com/godwit-email/godwit/core/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The fork adds no numbered migration: one merged into schema.go by
// mistake would take a number upstream will use for its own (FORK.md).
// Rebasing on a newer upstream release moves ForkBase to its count.
func TestMigrationsAreUpstreams(t *testing.T) {
	assert.Len(t, store.Migrations, store.ForkBase, "a numbered migration belongs in store.ForkMigrations (FORK.md)")
	require.NoError(t, store.CheckForkMigrations())
}

func userVersion(t *testing.T, db *store.DB) int64 {
	t.Helper()
	rows, err := db.Query(t.Context(), "PRAGMA user_version")
	require.NoError(t, err)
	return store.Int(rows.Values[0][0])
}

func hasObject(t *testing.T, db *store.DB, q string, args ...any) bool {
	t.Helper()
	rows, err := db.Query(t.Context(), q, args...)
	require.NoError(t, err)
	return len(rows.Values) > 0
}

func hasColumn(t *testing.T, db *store.DB, table, col string) bool {
	return hasObject(t, db, "SELECT 1 FROM pragma_table_info(?) WHERE name = ?", table, col)
}

func hasTable(t *testing.T, db *store.DB, name string) bool {
	return hasObject(t, db, "SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = ?", name)
}

// A database the fork opened keeps upstream's user_version: upstream's
// release that opens it next runs all of its own migrations, and the stock
// release it came from still opens it.
func TestForkKeepsUpstreamVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "godwit.db")
	db, err := store.Open(t.Context(), path)
	require.NoError(t, err)
	assert.Equal(t, int64(len(store.Migrations)), userVersion(t, db))
	assert.True(t, hasColumn(t, db, "drafts", "headers"))
	assert.True(t, hasTable(t, db, "server_drafts"))
	require.NoError(t, db.Close())

	all := store.ForkMigrations
	store.ForkMigrations = nil // the stock release
	defer func() { store.ForkMigrations = all }()
	db, err = store.Open(t.Context(), path)
	require.NoError(t, err, "the stock release opens a fork database")
	require.NoError(t, db.Close())
}

// Steps are applied every Open when their object is missing, never
// recorded: a column lost to a rollback or a table rebuild comes back, and a
// second Open changes nothing.
func TestForkStepsComeBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "godwit.db")
	db, err := store.Open(t.Context(), path)
	require.NoError(t, err)
	_, err = db.Execute(t.Context(), "ALTER TABLE drafts DROP COLUMN read_receipt")
	require.NoError(t, err)
	_, err = db.Execute(t.Context(), "DROP TABLE server_drafts")
	require.NoError(t, err)
	require.NoError(t, db.Close())

	db, err = store.Open(t.Context(), path)
	require.NoError(t, err)
	assert.True(t, hasColumn(t, db, "drafts", "read_receipt"))
	assert.True(t, hasTable(t, db, "server_drafts"))
	assert.True(t, hasObject(t, db, "SELECT 1 FROM sqlite_master WHERE type = 'index' AND name = 'server_drafts_draft'"))
	require.NoError(t, db.Close())

	db, err = store.Open(t.Context(), path)
	require.NoError(t, err)
	require.NoError(t, db.Close())
}

// Upstream adopting a fork feature in its next numbered migration, under
// the same names: the database the fork made has the objects already, so
// that migration skips creating them, runs the rest, and the database
// reaches upstream's new version.
func TestUpstreamAdoptsForkObjects(t *testing.T) {
	path := filepath.Join(t.TempDir(), "godwit.db")
	db, err := store.Open(t.Context(), path)
	require.NoError(t, err)
	_, err = db.Execute(t.Context(), "INSERT INTO accounts(id, email) VALUES (1, 'me@example.com')")
	require.NoError(t, err)
	_, err = db.Execute(t.Context(), `INSERT INTO drafts(id, account_id, mode, headers) VALUES (1, 1, 'new', '[{"Name":"X-A","Value":"1"}]')`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	all, base := store.Migrations, store.ForkBase
	defer func() { store.Migrations, store.ForkBase = all, base }()
	next := append(all[:len(all):len(all)], []string{
		`ALTER TABLE drafts ADD COLUMN headers TEXT NOT NULL DEFAULT '[]'`,
		`CREATE TABLE server_drafts (id INTEGER PRIMARY KEY)`,
		`CREATE INDEX server_drafts_draft ON server_drafts(id)`,
		`ALTER TABLE drafts ADD COLUMN upstream_only INTEGER NOT NULL DEFAULT 7`,
	})
	store.Migrations, store.ForkBase = next, len(next)
	db, err = store.Open(t.Context(), path)
	require.NoError(t, err)
	defer db.Close()
	assert.Equal(t, int64(len(next)), userVersion(t, db))
	rows, err := db.Query(t.Context(), "SELECT headers, upstream_only FROM drafts WHERE id = 1")
	require.NoError(t, err)
	assert.Equal(t, `[{"Name":"X-A","Value":"1"}]`, store.Str(rows.Values[0][0]), "the fork's data is kept")
	assert.Equal(t, int64(7), store.Int(rows.Values[0][1]), "upstream's own statements still run")
	assert.True(t, hasColumn(t, db, "server_drafts", "token"), "the fork's table is the one kept")
}

// A statement no fork step declares is never skipped: upstream's
// migrations fail exactly as upstream's would.
func TestUpstreamObjectsNeverSkipped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "godwit.db")
	db, err := store.Open(t.Context(), path)
	require.NoError(t, err)
	_, err = db.Execute(t.Context(), "ALTER TABLE drafts ADD COLUMN stray INTEGER")
	require.NoError(t, err)
	require.NoError(t, db.Close())

	all, base := store.Migrations, store.ForkBase
	defer func() { store.Migrations, store.ForkBase = all, base }()
	next := append(all[:len(all):len(all)], []string{`ALTER TABLE drafts ADD COLUMN stray INTEGER`})
	store.Migrations, store.ForkBase = next, len(next)
	_, err = store.Open(t.Context(), path)
	require.ErrorContains(t, err, "duplicate column")
}

// Fork steps hold only statements Open can tell are applied.
func TestForkStepsAdditiveOnly(t *testing.T) {
	all := store.ForkMigrations
	defer func() { store.ForkMigrations = all }()
	store.ForkMigrations = []store.ForkMigration{{Name: "bad", Stmts: []string{"UPDATE drafts SET subject = ''"}}}
	require.ErrorContains(t, store.CheckForkMigrations(), "not an ADD COLUMN")
	store.ForkMigrations = []store.ForkMigration{{Name: "a", Stmts: nil}, {Name: "a", Stmts: nil}}
	require.ErrorContains(t, store.CheckForkMigrations(), "used twice")
}

func indexSQL(t *testing.T, db *store.DB, name string) string {
	t.Helper()
	rows, err := db.Query(t.Context(), "SELECT sql FROM sqlite_master WHERE type = 'index' AND name = ?", name)
	require.NoError(t, err)
	if len(rows.Values) == 0 {
		return ""
	}
	return store.Str(rows.Values[0][0])
}

// The count and date indexes (perf-indexes) are built at the first open,
// a later open leaves them as they are, and one dropped comes back.
func TestForkPerfIndexes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "godwit.db")
	db, err := store.Open(t.Context(), path)
	require.NoError(t, err)
	unseen, date := indexSQL(t, db, "messages_unseen"), indexSQL(t, db, "threads_last_date")
	assert.Contains(t, unseen, `WHERE instr(flags, '\Seen') = 0`)
	assert.Contains(t, date, "threads(last_date DESC, id DESC)")
	_, err = db.Execute(t.Context(), "INSERT INTO accounts(id, email) VALUES (1, 'me@example.com')")
	require.NoError(t, err)
	_, err = db.Execute(t.Context(), `INSERT INTO messages(account_id, flags) VALUES (1, ''), (1, '\Seen'), (1, '\Flagged')`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	db, err = store.Open(t.Context(), path)
	require.NoError(t, err, "a second open changes nothing")
	assert.Equal(t, unseen, indexSQL(t, db, "messages_unseen"))
	assert.Equal(t, date, indexSQL(t, db, "threads_last_date"))
	_, err = db.Execute(t.Context(), "DROP INDEX messages_unseen")
	require.NoError(t, err)
	_, err = db.Execute(t.Context(), "DROP INDEX threads_last_date")
	require.NoError(t, err)
	require.NoError(t, db.Close())

	db, err = store.Open(t.Context(), path)
	require.NoError(t, err)
	defer db.Close()
	assert.Equal(t, unseen, indexSQL(t, db, "messages_unseen"), "a dropped index comes back")
	assert.Equal(t, date, indexSQL(t, db, "threads_last_date"))
	rows, err := db.Query(t.Context(), `SELECT count(*) FROM messages INDEXED BY messages_unseen WHERE account_id = 1 AND instr(flags, '\Seen') = 0`)
	require.NoError(t, err)
	assert.Equal(t, int64(2), store.Int(rows.Values[0][0]), "the rebuilt index holds the unread messages")
}
