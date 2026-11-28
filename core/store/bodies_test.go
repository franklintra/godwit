package store_test

import (
	"context"
	"crypto/sha3"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/plugins/sqlite"
	"github.com/godwit-email/godwit/core/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// body is a message's two body columns as stock Godwit stores them: nil
// (NULL) or a string.
type body [2]any

// stockDB is a database stock Godwit wrote: upstream's schema and
// triggers, the fork's additive steps, every body inline. It holds n
// messages with every body shape: none, an empty text, text only, HTML
// only, both, and large ones.
func stockDB(t *testing.T, n int) (string, map[int64]body) {
	t.Helper()
	all := store.ForkRewrites
	store.ForkRewrites = nil
	defer func() { store.ForkRewrites = all }()
	path := filepath.Join(t.TempDir(), "godwit.db")
	db, err := store.Open(ctx, path)
	require.NoError(t, err)
	defer db.Close()
	exec(t, db, "INSERT INTO accounts(id, email) VALUES (1, 'a@b.c')")
	r := rand.New(rand.NewPCG(7, 53))
	words := []string{"alpha", "bravo", "charlie", "delta", "écho", "фокстрот", "高尔夫", "hotel"}
	text := func(size int) string {
		var b strings.Builder
		for b.Len() < size {
			b.WriteString(words[r.IntN(len(words))])
			b.WriteByte(' ')
		}
		return b.String()
	}
	want := map[int64]body{}
	for i := range n {
		var b body
		switch i % 7 {
		case 0: // no body
		case 1:
			b[0] = ""
		case 2:
			b[0] = text(200 + r.IntN(4000))
		case 3:
			b[1] = "<p>" + text(500+r.IntN(9000)) + "</p>"
		case 4, 5:
			b[0], b[1] = text(100+r.IntN(3000)), "<div>"+text(1000+r.IntN(30000))+"</div>"
		case 6:
			b[0], b[1] = text(150_000), "<p>"+text(400_000)+"</p>"
		}
		res, err := db.Execute(ctx, "INSERT INTO messages(account_id, subject, from_addr, body_text, body_html) VALUES (1, ?, ?, ?, ?)",
			fmt.Sprintf("subject %d %s", i, words[i%len(words)]), fmt.Sprintf("sender%d@example.com", i), b[0], b[1])
		require.NoError(t, err)
		want[res.LastInsertID] = b
	}
	return path, want
}

func exec(t *testing.T, db *store.DB, sql string, args ...any) {
	t.Helper()
	_, err := db.Execute(ctx, sql, args...)
	require.NoError(t, err, sql)
}

func openPath(t *testing.T, path string) *store.DB {
	t.Helper()
	db, err := store.Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	return db
}

func bodiesRewrite(t *testing.T) store.ForkRewrite {
	r, ok := store.Rewrite("bodies")
	require.True(t, ok)
	return r
}

// readBodies is every message's body as the readers see it.
func readBodies(t *testing.T, db *store.DB) map[int64]body {
	t.Helper()
	rows, err := db.Query(ctx, "SELECT id, "+store.BodyTextSQL("m")+", "+store.BodyHTMLSQL("m")+" FROM messages m")
	require.NoError(t, err)
	out := map[int64]body{}
	for _, r := range rows.Values {
		out[store.Int(r[0])] = body{r[1], r[2]}
	}
	return out
}

// inlineBodies is every message's inline columns, as stock Godwit reads them.
func inlineBodies(t *testing.T, db *store.DB) map[int64]body {
	t.Helper()
	rows, err := db.Query(ctx, "SELECT id, body_text, body_html FROM messages")
	require.NoError(t, err)
	out := map[int64]body{}
	for _, r := range rows.Values {
		out[store.Int(r[0])] = body{r[1], r[2]}
	}
	return out
}

func countOf(t *testing.T, db *store.DB, sql string) int64 {
	t.Helper()
	rows, err := db.Query(ctx, sql)
	require.NoError(t, err, sql)
	return store.Int(rows.Values[0][0])
}

// onePlace checks that no message holds a body both inline and in
// message_bodies, and that every body reads as want.
func onePlace(t *testing.T, db *store.DB, want map[int64]body) {
	t.Helper()
	assert.Zero(t, countOf(t, db, "SELECT count(*) FROM messages m JOIN message_bodies b ON b.message_id = m.id WHERE m.body_text IS NOT NULL OR m.body_html IS NOT NULL"), "a body in both places")
	assert.Equal(t, want, readBodies(t, db), "a body lost or changed")
}

// ftsHash is a SHA3 of the full-text index's shadow tables: its content
// copy and its index, byte for byte.
func ftsHash(t *testing.T, db *store.DB) string {
	t.Helper()
	h := sha3.New256()
	for _, tb := range []string{"messages_fts_data", "messages_fts_idx", "messages_fts_content", "messages_fts_docsize", "messages_fts_config"} {
		rows, err := db.Query(ctx, "SELECT * FROM "+tb+" ORDER BY 1, 2")
		require.NoError(t, err)
		fmt.Fprintf(h, "%s %d\n", tb, len(rows.Values))
		for _, r := range rows.Values {
			for _, v := range r {
				fmt.Fprintf(h, "%T:%v\x00", v, v)
			}
		}
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

func ftsIntact(t *testing.T, db *store.DB) {
	t.Helper()
	_, err := db.Execute(ctx, "INSERT INTO messages_fts(messages_fts) VALUES ('integrity-check')")
	require.NoError(t, err, "FTS integrity-check")
	rows, err := db.Query(ctx, "PRAGMA quick_check")
	require.NoError(t, err)
	require.Equal(t, "ok", store.Str(rows.Values[0][0]))
}

func state(t *testing.T, db *store.DB) string {
	t.Helper()
	s, err := db.RewriteState(ctx, "bodies")
	require.NoError(t, err)
	return s
}

func withBody(want map[int64]body) int64 {
	n := int64(0)
	for _, b := range want {
		if b[0] != nil || b[1] != nil {
			n++
		}
	}
	return n
}

// A database stock Godwit wrote: the fork's open makes the table and
// triggers and touches no body; batches then move bodies with every body
// in one place after each; a stop after some batches loses nothing and the
// next open goes on; at the end no body is inline, every body reads as
// before, and the full-text index is byte for byte the same.
func TestBodiesMoveResumes(t *testing.T) {
	path, want := stockDB(t, 63)
	before := func() string {
		all := store.ForkRewrites
		store.ForkRewrites = nil
		defer func() { store.ForkRewrites = all }()
		db := openPath(t, path)
		defer db.Close()
		ftsIntact(t, db)
		return ftsHash(t, db)
	}()
	r := bodiesRewrite(t)

	db := openPath(t, path)
	assert.Equal(t, store.RewriteMoving, state(t, db))
	assert.Zero(t, countOf(t, db, "SELECT count(*) FROM message_bodies"), "Open moves nothing")
	onePlace(t, db, want)
	for range 3 {
		res, err := r.Backfill(ctx, db, store.Batch{Rows: 5, Bytes: 64 << 10})
		require.NoError(t, err)
		require.True(t, res.More)
		assert.Positive(t, res.Rows)
		onePlace(t, db, want)
	}
	inline := countOf(t, db, "SELECT count(*) FROM messages WHERE body_text IS NOT NULL OR body_html IS NOT NULL")
	assert.Positive(t, inline, "the move stopped half way")
	require.NoError(t, db.Close())

	db = openPath(t, path) // the next start
	assert.Equal(t, store.RewriteMoving, state(t, db))
	onePlace(t, db, want)
	batches := 0
	for {
		res, err := r.Backfill(ctx, db, store.Batch{Rows: 9, Bytes: 256 << 10})
		require.NoError(t, err)
		onePlace(t, db, want)
		if !res.More {
			break
		}
		batches++
		require.Less(t, batches, 200)
	}
	assert.Equal(t, store.RewriteDone, state(t, db))
	assert.Zero(t, countOf(t, db, "SELECT count(*) FROM messages WHERE body_text IS NOT NULL OR body_html IS NOT NULL"))
	assert.Equal(t, withBody(want), countOf(t, db, "SELECT count(*) FROM message_bodies"))
	assert.Equal(t, before, ftsHash(t, db), "the move does not touch the full-text index")
	ftsIntact(t, db)
	res, err := r.Backfill(ctx, db, store.Batch{Rows: 9, Bytes: 1 << 20})
	require.NoError(t, err)
	assert.False(t, res.More, "done stays done")
}

// One large body is moved alone: the byte bound never stops a batch
// before its first row.
func TestBodiesBatchBytes(t *testing.T) {
	path, want := stockDB(t, 7)
	db := openPath(t, path)
	r := bodiesRewrite(t)
	for {
		res, err := r.Backfill(ctx, db, store.Batch{Rows: 50, Bytes: 1})
		require.NoError(t, err)
		if !res.More {
			break
		}
		assert.LessOrEqual(t, res.Rows, 1)
	}
	onePlace(t, db, want)
}

// A batch interrupted at any point (its context ends) commits all of its
// rows or none of them.
func TestBodiesMoveCancelled(t *testing.T) {
	path, want := stockDB(t, 70)
	db := openPath(t, path)
	r := bodiesRewrite(t)
	interrupted := 0
	for i := 0; ; i++ {
		c, cancel := context.WithTimeout(ctx, time.Duration(i%12)*150*time.Microsecond)
		res, err := r.Backfill(c, db, store.Batch{Rows: 6, Bytes: 4 << 20})
		cancel()
		onePlace(t, db, want)
		if err != nil {
			interrupted++
			continue
		}
		if !res.More {
			break
		}
		require.Less(t, i, 2000)
	}
	assert.Positive(t, interrupted, "some batches were interrupted")
	assert.Equal(t, store.RewriteDone, state(t, db))
	ftsIntact(t, db)
}

// The fork's triggers come back when an upstream migration redefines one
// (here messages_au, as upstream's first migration made it) or drops one,
// and the move starts over: a writer may have stored bodies inline
// meanwhile. An Open with every trigger in place changes nothing.
func TestBodiesTriggersComeBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "godwit.db")
	db := openPath(t, path)
	r := bodiesRewrite(t)
	res, err := r.Backfill(ctx, db, store.Batch{Rows: 10, Bytes: 1 << 20})
	require.NoError(t, err)
	require.False(t, res.More, "a new database has nothing to move")
	schema := func() string {
		rows, err := db.Query(ctx, "SELECT group_concat(type || name || sql, '|') FROM (SELECT * FROM sqlite_master ORDER BY name)")
		require.NoError(t, err)
		meta, err := db.Query(ctx, "SELECT group_concat(key || '=' || value) FROM meta WHERE key LIKE 'fork.%'")
		require.NoError(t, err)
		return store.Str(rows.Values[0][0]) + store.Str(meta.Values[0][0])
	}
	done := schema()
	require.NoError(t, db.Close())
	db = openPath(t, path)
	assert.Equal(t, done, schema(), "an Open of a database that is up to date changes nothing")

	exec(t, db, "INSERT INTO accounts(id, email) VALUES (1, 'a@b.c')")
	exec(t, db, "DROP TRIGGER messages_au")
	exec(t, db, upstreamAU(t))
	exec(t, db, "DROP TRIGGER messages_body_move")
	exec(t, db, "INSERT INTO messages(id, account_id, subject, from_addr, body_text, body_html) VALUES (5, 1, 'kilo', 'x@y.z', 'lima mike', '<p>november</p>')")
	assert.Equal(t, int64(1), countOf(t, db, "SELECT count(*) FROM messages WHERE body_text IS NOT NULL"), "stored inline while the trigger was gone")
	require.NoError(t, db.Close())

	db = openPath(t, path)
	assert.Equal(t, store.RewriteMoving, state(t, db))
	for {
		res, err := r.Backfill(ctx, db, store.Batch{Rows: 10, Bytes: 1 << 20})
		require.NoError(t, err)
		if !res.More {
			break
		}
	}
	onePlace(t, db, map[int64]body{5: {"lima mike", "<p>november</p>"}})
	assert.Equal(t, done, schema(), "the fork's triggers again, state done")
	exec(t, db, "UPDATE messages SET subject = 'oscar' WHERE id = 5")
	assert.Equal(t, int64(1), countOf(t, db, "SELECT count(*) FROM messages_fts WHERE messages_fts MATCH 'mike AND oscar'"), "a subject change keeps the body indexed")
	ftsIntact(t, db)
}

// upstreamAU is messages_au as upstream's migrations made it last.
func upstreamAU(t *testing.T) string {
	t.Helper()
	last := ""
	for _, m := range store.Migrations {
		for _, s := range m {
			if regexp.MustCompile(`(?i)^\s*CREATE\s+TRIGGER\s+messages_au\b`).MatchString(s) {
				last = s
			}
		}
	}
	require.NotEmpty(t, last)
	return last
}

func sameSQL(a, b string) bool {
	sp := regexp.MustCompile(`\s+`)
	return sp.ReplaceAllString(strings.TrimSpace(a), " ") == sp.ReplaceAllString(strings.TrimSpace(b), " ")
}

// fork-undo: every body back inline, the stock schema back (upstream's
// messages_au, no message_bodies, no fork trigger), the full-text index
// untouched; stock Godwit then reads every body as before. The fork's next
// open moves them again.
func TestBodiesUndo(t *testing.T) {
	path, want := stockDB(t, 49)
	db := openPath(t, path)
	ftsBefore := ftsHash(t, db)
	r := bodiesRewrite(t)
	for {
		res, err := r.Backfill(ctx, db, store.Batch{Rows: 8, Bytes: 1 << 20})
		require.NoError(t, err)
		if !res.More {
			break
		}
	}
	var progress []int
	require.NoError(t, r.Undo(ctx, db, func(n int) { progress = append(progress, n) }))
	assert.NotEmpty(t, progress)
	require.NoError(t, db.Close())

	all := store.ForkRewrites
	store.ForkRewrites = nil // stock Godwit
	db = openPath(t, path)
	store.ForkRewrites = all
	assert.Equal(t, want, inlineBodies(t, db), "stock Godwit reads every body inline")
	assert.False(t, hasTable(t, db, "message_bodies"))
	rows, err := db.Query(ctx, "SELECT name, sql FROM sqlite_master WHERE type = 'trigger' AND (tbl_name IN ('messages', 'message_bodies'))")
	require.NoError(t, err)
	for _, row := range rows.Values {
		name := store.Str(row[0])
		assert.NotContains(t, []string{"message_bodies_ai", "message_bodies_au", "messages_body_move", "messages_body_move_au"}, name)
		if name == "messages_au" {
			assert.True(t, sameSQL(upstreamAU(t), store.Str(row[1])), "upstream's messages_au is back")
		}
	}
	meta, err := db.Query(ctx, "SELECT count(*) FROM meta WHERE key LIKE 'fork.bodies%'")
	require.NoError(t, err)
	assert.Zero(t, store.Int(meta.Values[0][0]))
	assert.Equal(t, ftsBefore, ftsHash(t, db))
	ftsIntact(t, db)
	exec(t, db, "UPDATE messages SET body_text = 'papa quebec' WHERE id = (SELECT min(id) FROM messages)")
	assert.Equal(t, int64(1), countOf(t, db, "SELECT count(*) FROM messages_fts WHERE messages_fts MATCH 'quebec'"), "upstream's trigger indexes an inline change")
	require.NoError(t, db.Close())

	db = openPath(t, path) // the fork again
	assert.Equal(t, store.RewriteMoving, state(t, db))
	for {
		res, err := r.Backfill(ctx, db, store.Batch{Rows: 8, Bytes: 1 << 20})
		require.NoError(t, err)
		if !res.More {
			break
		}
	}
	assert.Zero(t, countOf(t, db, "SELECT count(*) FROM messages WHERE body_text IS NOT NULL OR body_html IS NOT NULL"))
	ftsIntact(t, db)
}

// A writer that still stores bodies inline (stock code, tests) ends with
// them in message_bodies, the index right, and a column it leaves out
// keeps its value. The sync writers' path (NULL inline, then the body row)
// indexes the body too.
func TestBodiesSafetyTriggers(t *testing.T) {
	db := open(t)
	match := func(q string) int64 {
		return countOf(t, db, "SELECT count(*) FROM messages_fts WHERE messages_fts MATCH '"+q+"'")
	}
	exec(t, db, "INSERT INTO accounts(id, email) VALUES (1, 'a@b.c')")
	exec(t, db, "INSERT INTO messages(id, account_id, subject, from_addr, body_text, body_html) VALUES (1, 1, 'hello', 'x@y.z', 'alpha content', '<p>alpha</p>')")
	onePlace(t, db, map[int64]body{1: {"alpha content", "<p>alpha</p>"}})
	assert.Equal(t, int64(1), match("alpha"))

	exec(t, db, "UPDATE messages SET body_html = '<p>beta</p>' WHERE id = 1")
	onePlace(t, db, map[int64]body{1: {"alpha content", "<p>beta</p>"}})
	assert.Equal(t, int64(1), match("alpha"), "an HTML change keeps the text indexed")

	exec(t, db, "UPDATE messages SET body_text = 'bravo content' WHERE id = 1")
	onePlace(t, db, map[int64]body{1: {"bravo content", "<p>beta</p>"}})
	assert.Equal(t, int64(1), match("bravo"))
	assert.Zero(t, match("alpha"))

	exec(t, db, "UPDATE messages SET subject = 'renamed' WHERE id = 1")
	assert.Equal(t, int64(1), match("bravo"), "a subject change keeps the body")
	assert.Equal(t, int64(1), match("renamed"))

	_, err := db.Transaction(ctx, []sqlite.Statement{
		{SQL: "INSERT INTO messages(id, account_id, subject, from_addr) VALUES (2, 1, 'sync', 's@y.z')"},
		{SQL: "INSERT INTO message_bodies(message_id, body_text, body_html) VALUES (last_insert_rowid(), 'charlie text', NULL)"},
	})
	require.NoError(t, err)
	onePlace(t, db, map[int64]body{1: {"bravo content", "<p>beta</p>"}, 2: {"charlie text", nil}})
	assert.Equal(t, int64(1), match("charlie"), "the sync writers' path indexes the body")
	exec(t, db, "UPDATE message_bodies SET body_text = 'delta text' WHERE message_id = 2")
	assert.Equal(t, int64(1), match("delta"))
	assert.Zero(t, match("charlie"))
	ftsIntact(t, db)

	exec(t, db, "DELETE FROM messages WHERE id = 1")
	assert.Zero(t, countOf(t, db, "SELECT count(*) FROM message_bodies WHERE message_id = 1"), "the body goes with its message")
	assert.Zero(t, match("bravo"))
	ftsIntact(t, db)
}

// A row in message_bodies whose message also got an inline body (written
// while the triggers were gone) takes the inline value, the newer one, and
// keeps the stored column the inline write left NULL.
func TestBodiesInlineOverStaleRow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "godwit.db")
	db := openPath(t, path)
	exec(t, db, "INSERT INTO accounts(id, email) VALUES (1, 'a@b.c')")
	exec(t, db, "INSERT INTO messages(id, account_id, subject, from_addr, body_text, body_html) VALUES (1, 1, 's', 'x@y.z', 'old text', '<p>old</p>')")
	for _, n := range []string{"messages_body_move", "messages_body_move_au", "messages_au"} {
		exec(t, db, "DROP TRIGGER "+n)
	}
	exec(t, db, upstreamAU(t))
	exec(t, db, "UPDATE messages SET body_text = 'new text' WHERE id = 1")
	require.NoError(t, db.Close())
	db = openPath(t, path)
	r := bodiesRewrite(t)
	for {
		res, err := r.Backfill(ctx, db, store.Batch{Rows: 10, Bytes: 1 << 20})
		require.NoError(t, err)
		if !res.More {
			break
		}
	}
	onePlace(t, db, map[int64]body{1: {"new text", "<p>old</p>"}})
	ftsIntact(t, db)
}

// The cost of the safety triggers against the sync writers' path, per
// message, in time and WAL bytes. GODWIT_BODY_WRITECOST=1 runs it; it
// only reports.
func TestBodyWriteCost(t *testing.T) {
	if os.Getenv("GODWIT_BODY_WRITECOST") == "" {
		t.Skip("GODWIT_BODY_WRITECOST=1 measures the write paths")
	}
	store.SetTestingForTest(t, false) // synchronous = NORMAL, as the app
	html := "<p>" + strings.Repeat("lorem ipsum dolor sit amet ", 1500) + "</p>"
	text := strings.Repeat("lorem ipsum dolor sit amet ", 150)
	const n = 1500
	all := store.ForkRewrites
	defer func() { store.ForkRewrites = all }()
	for _, path := range []string{"stock", "direct", "safety"} {
		store.ForkRewrites = all
		if path == "stock" { // stock Godwit: inline bodies, upstream's triggers
			store.ForkRewrites = nil
		}
		dir := t.TempDir()
		db, err := store.Open(ctx, filepath.Join(dir, "godwit.db"))
		require.NoError(t, err)
		exec(t, db, "INSERT INTO accounts(id, email) VALUES (1, 'a@b.c')")
		_, err = db.Query(ctx, "PRAGMA wal_autocheckpoint = 0")
		require.NoError(t, err)
		_, err = db.Query(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
		require.NoError(t, err)
		start := time.Now()
		for i := range n {
			var stmts []sqlite.Statement
			if path == "direct" {
				stmts = []sqlite.Statement{
					{SQL: "INSERT INTO messages(account_id, subject, from_addr) VALUES (1, ?, 'x@y.z')", Args: []any{fmt.Sprint("s", i)}},
					{SQL: "INSERT INTO message_bodies(message_id, body_text, body_html) VALUES (last_insert_rowid(), ?, ?)", Args: []any{text, html}},
				}
			} else {
				stmts = []sqlite.Statement{{SQL: "INSERT INTO messages(account_id, subject, from_addr, body_text, body_html) VALUES (1, ?, 'x@y.z', ?, ?)", Args: []any{fmt.Sprint("s", i), text, html}}}
			}
			_, err := db.Transaction(ctx, stmts)
			require.NoError(t, err)
		}
		took := time.Since(start)
		fi, err := os.Stat(filepath.Join(dir, "godwit.db-wal"))
		require.NoError(t, err)
		t.Logf("%s path: %.2f ms and %.1f KB of WAL per message (%d messages, body %d KB)", path,
			float64(took.Microseconds())/1000/n, float64(fi.Size())/1024/n, n, (len(text)+len(html))/1024)
		require.NoError(t, db.Close())
	}
}
