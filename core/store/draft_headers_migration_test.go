package store_test

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/godwit-email/godwit/core/store"
	"github.com/stretchr/testify/require"
)

// Drafts stored before drafts.headers existed read as having none.
func TestMigrationDraftHeaders(t *testing.T) {
	all := store.Migrations
	// Up to the migration under test, which need not stay the last one.
	i := slices.IndexFunc(all, func(m []string) bool {
		return strings.Contains(strings.Join(m, "\n"), "ALTER TABLE drafts ADD COLUMN headers")
	})
	require.Positive(t, i)
	store.Migrations = all[:i]
	defer func() { store.Migrations = all }()
	path := filepath.Join(t.TempDir(), "drafts.db")
	db, err := store.Open(t.Context(), path)
	require.NoError(t, err)
	_, err = db.Execute(t.Context(), "INSERT INTO accounts(id,email) VALUES (1,'me@example.com')")
	require.NoError(t, err)
	_, err = db.Execute(t.Context(), "INSERT INTO drafts(id,account_id,mode,subject) VALUES (1,1,'new','old')")
	require.NoError(t, err)
	require.NoError(t, db.Close())

	store.Migrations = all
	db, err = store.Open(t.Context(), path)
	require.NoError(t, err)
	defer db.Close()
	rows, err := db.Query(t.Context(), "SELECT headers, subject FROM drafts WHERE id = 1")
	require.NoError(t, err)
	require.Equal(t, "[]", store.Str(rows.Values[0][0]))
	require.Equal(t, "old", store.Str(rows.Values[0][1]))
}
