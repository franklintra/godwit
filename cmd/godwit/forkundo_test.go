package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/egoist/mygo/plugins/sqlite"
	"github.com/godwit-email/godwit/core/store"
	"github.com/godwit-email/godwit/ownerlock"
)

// fork-undo bodies puts the bodies back inline, where stock Godwit reads
// them, and refuses while Godwit runs on the data directory.
func TestForkUndoBodies(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(dir, "godwit.db"))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		"INSERT INTO accounts(id, email) VALUES (1, 'a@b.c')",
		"INSERT INTO messages(id, account_id, subject, body_text, body_html) VALUES (1, 1, 's', 'alpha', '<p>alpha</p>')",
	} {
		if _, err := db.Execute(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	release, err := ownerlock.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := run(ctx, []string{"fork-undo", "bodies", "--data-dir", dir}, nil, &out, &errb); code != 1 || !strings.Contains(errb.String(), "quit Godwit") {
		t.Fatalf("while Godwit runs: exit %d, %q", code, errb.String())
	}
	release()
	if code := run(ctx, []string{"fork-undo", "nothing", "--data-dir", dir}, nil, &out, &errb); code != 2 {
		t.Fatalf("an unknown rewrite: exit %d", code)
	}
	errb.Reset()
	if code := run(ctx, []string{"fork-undo", "bodies", "--data-dir", dir}, nil, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}

	raw, err := sqlite.Open(ctx, filepath.Join(dir, "godwit.db"), sqlite.OpenOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	rows, err := raw.Query(ctx, "SELECT body_text, body_html, (SELECT count(*) FROM sqlite_master WHERE name = 'message_bodies') FROM messages")
	if err != nil {
		t.Fatal(err)
	}
	if got := rows.Values[0]; store.Str(got[0]) != "alpha" || store.Str(got[1]) != "<p>alpha</p>" || store.Int(got[2]) != 0 {
		t.Fatalf("after fork-undo: %v", got)
	}
}
