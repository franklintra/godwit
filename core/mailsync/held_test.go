package mailsync

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/godwit-email/godwit/backend"
	"github.com/godwit-email/godwit/core/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Held reads the server_ids from messages_server_id, not the message rows,
// and answers what the join of locations and messages answered: every
// location with a server_ref, "" when its message has no server_id.
func TestHeldMatchesJoin(t *testing.T) {
	ctx := t.Context()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "godwit.db"))
	require.NoError(t, err)
	defer db.Close()
	for _, q := range []string{
		`INSERT INTO accounts(id, email) VALUES (1, 'a@x'), (2, 'b@x')`,
		`INSERT INTO folders(id, account_id, name, role) VALUES (10, 1, 'INBOX', 'inbox'), (11, 1, 'All', 'all'), (12, 1, 'Empty', ''), (20, 2, 'INBOX', 'inbox')`,
		`INSERT INTO messages(id, account_id, server_id) VALUES (1, 1, '111'), (2, 1, NULL), (3, 1, '333'), (4, 1, '444'), (5, 2, '555'), (6, 2, NULL)`,
		`INSERT INTO message_locations(message_id, folder_id, shown, server_ref, entered_at) VALUES
			(1, 10, 1, 'r1', 0), (2, 10, 1, 'r2', 0), (3, 10, 0, 'r3', 0), (4, 10, 1, NULL, 0),
			(1, 11, 1, 'a1', 0), (4, 11, 1, 'a4', 0), (5, 20, 1, 'r5', 0), (6, 20, 1, 'r6', 0)`,
	} {
		_, err := db.Execute(ctx, q)
		require.NoError(t, err, q)
	}
	for _, c := range []struct {
		acc, folder int64
	}{{1, 10}, {1, 11}, {1, 12}, {2, 20}} {
		s := &syncRun{db: db, a: &account{id: c.acc}, folders: map[backend.FolderRef]*folder{"F": {id: c.folder, ref: "F"}}}
		got, err := s.Held(ctx, "F")
		require.NoError(t, err)
		rows, err := db.Query(ctx, `SELECT l.server_ref, m.server_id FROM message_locations l JOIN messages m ON m.id = l.message_id
			WHERE l.folder_id = ? AND l.server_ref IS NOT NULL`, c.folder)
		require.NoError(t, err)
		want := map[backend.MessageRef]string{}
		for _, v := range rows.Values {
			want[backend.MessageRef(store.Str(v[0]))] = store.Str(v[1])
		}
		assert.Equal(t, want, got, "folder %d", c.folder)
	}
	s := &syncRun{db: db, a: &account{id: 1}, folders: map[backend.FolderRef]*folder{}}
	got, err := s.Held(ctx, "missing")
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestHeldIDsPlan(t *testing.T) {
	ctx := t.Context()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "godwit.db"))
	require.NoError(t, err)
	defer db.Close()
	rows, err := db.Query(ctx, "EXPLAIN QUERY PLAN "+heldIDs, 1, 10)
	require.NoError(t, err)
	var lines []string
	for _, r := range rows.Values {
		lines = append(lines, store.Str(r[len(r)-1]))
	}
	p := strings.Join(lines, "\n")
	t.Logf("heldIDs:\n%s", p)
	assert.Contains(t, p, "SEARCH messages USING COVERING INDEX messages_server_id (account_id=?")
	assert.NotContains(t, p, "INTEGER PRIMARY KEY")
}
