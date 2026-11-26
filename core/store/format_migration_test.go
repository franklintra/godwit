package store_test

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/godwit-email/godwit/core/store"
	"github.com/stretchr/testify/require"
)

// Drafts and accounts stored before draft formats existed read as HTML
// drafts and as accounts that follow the settings' default.
func TestMigrationDraftFormat(t *testing.T) {
	all := store.Migrations
	i := slices.IndexFunc(all, func(m []string) bool { return strings.Contains(strings.Join(m, "\n"), "ADD COLUMN compose_format") })
	require.Positive(t, i)
	store.Migrations = all[:i]
	defer func() { store.Migrations = all }()
	path := filepath.Join(t.TempDir(), "format.db")
	db, err := store.Open(t.Context(), path)
	require.NoError(t, err)
	_, err = db.Execute(t.Context(), "INSERT INTO accounts(id,email) VALUES (1,'me@example.com')")
	require.NoError(t, err)
	_, err = db.Execute(t.Context(), "INSERT INTO drafts(id,account_id,mode,body_html,updated_at) VALUES (1,1,'new','<p>hi</p>',1)")
	require.NoError(t, err)
	require.NoError(t, db.Close())
	store.Migrations = all
	db, err = store.Open(t.Context(), path)
	require.NoError(t, err)
	defer db.Close()
	rows, err := db.Query(t.Context(), "SELECT d.format, a.compose_format FROM drafts d JOIN accounts a ON a.id = d.account_id")
	require.NoError(t, err)
	require.Len(t, rows.Values, 1)
	require.Equal(t, "html", store.Str(rows.Values[0][0]))
	require.Equal(t, "", store.Str(rows.Values[0][1]))
}
