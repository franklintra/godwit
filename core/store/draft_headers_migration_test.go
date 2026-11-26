package store_test

import (
	"path/filepath"
	"testing"

	"github.com/godwit-email/godwit/core/store"
	"github.com/stretchr/testify/require"
)

// Drafts stored before drafts.headers existed read as having none. The
// column is a fork step (FORK.md), applied on top of upstream's schema.
func TestMigrationDraftHeaders(t *testing.T) {
	all := store.ForkMigrations
	store.ForkMigrations = nil // upstream's schema alone
	defer func() { store.ForkMigrations = all }()
	path := filepath.Join(t.TempDir(), "drafts.db")
	db, err := store.Open(t.Context(), path)
	require.NoError(t, err)
	_, err = db.Execute(t.Context(), "INSERT INTO accounts(id,email) VALUES (1,'me@example.com')")
	require.NoError(t, err)
	_, err = db.Execute(t.Context(), "INSERT INTO drafts(id,account_id,mode,subject) VALUES (1,1,'new','old')")
	require.NoError(t, err)
	require.NoError(t, db.Close())

	store.ForkMigrations = all
	db, err = store.Open(t.Context(), path)
	require.NoError(t, err)
	defer db.Close()
	rows, err := db.Query(t.Context(), "SELECT headers, read_receipt, subject FROM drafts WHERE id = 1")
	require.NoError(t, err)
	require.Equal(t, "[]", store.Str(rows.Values[0][0]))
	require.Equal(t, int64(0), store.Int(rows.Values[0][1]))
	require.Equal(t, "old", store.Str(rows.Values[0][2]))
}
