package core

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/godwit-email/godwit/core/internal/testmail"
	"github.com/godwit-email/godwit/core/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The fork moves mail bodies to message_bodies (ADR 0053). These tests
// read the same mail in both layouts, bodies inline (as stock Godwit wrote
// them, before the move) and bodies moved, and require the same results
// from every reader of a body.

// waitBodiesMoved waits for the service's background move to end.
func waitBodiesMoved(t *testing.T, s *Service) {
	t.Helper()
	require.Eventually(t, func() bool {
		st, err := s.db.RewriteState(bg, "bodies")
		return err == nil && st == store.RewriteDone
	}, 20*time.Second, 5*time.Millisecond)
}

// inlineLayout puts every body back inline under the fork's schema: the
// layout of a database stock Godwit wrote, which the move has not reached.
func inlineLayout(t *testing.T, s *Service) {
	t.Helper()
	waitBodiesMoved(t, s) // the background move is over: nothing moves the bodies back
	r, ok := store.Rewrite("bodies")
	require.True(t, ok)
	require.NoError(t, r.Undo(bg, s.db, nil))
	require.NoError(t, r.Ensure(bg, s.db))
	_, inline, stored := bodyPlaces(t, s)
	require.Positive(t, inline)
	require.Zero(t, stored)
}

// movedLayout runs the move to its end in small batches; after each, no
// body is in both places.
func movedLayout(t *testing.T, s *Service) {
	t.Helper()
	r, _ := store.Rewrite("bodies")
	for i := 0; ; i++ {
		res, err := r.Backfill(bg, s.db, store.Batch{Rows: 2, Bytes: 1 << 20})
		require.NoError(t, err)
		both, _, _ := bodyPlaces(t, s)
		require.Zero(t, both)
		if !res.More {
			break
		}
		require.Less(t, i, 10_000)
	}
	_, inline, stored := bodyPlaces(t, s)
	require.Zero(t, inline)
	require.Positive(t, stored)
}

// bodyPlaces counts messages with a body in both places, inline, and in
// message_bodies.
func bodyPlaces(t *testing.T, s *Service) (both, inline, stored int) {
	t.Helper()
	r, err := s.db.Query(bg, `SELECT
		(SELECT count(*) FROM messages m JOIN message_bodies b ON b.message_id = m.id WHERE m.body_text IS NOT NULL OR m.body_html IS NOT NULL),
		(SELECT count(*) FROM messages WHERE body_text IS NOT NULL OR body_html IS NOT NULL),
		(SELECT count(*) FROM message_bodies)`)
	require.NoError(t, err)
	return int(store.Int(r.Values[0][0])), int(store.Int(r.Values[0][1])), int(store.Int(r.Values[0][2]))
}

func ftsIntegrity(t *testing.T, s *Service) {
	t.Helper()
	_, err := s.db.Execute(bg, "INSERT INTO messages_fts(messages_fts) VALUES ('integrity-check')")
	require.NoError(t, err)
}

// bodyMails are messages of every body shape the sync stores.
func bodyMails(date string) map[string]string {
	big := strings.Repeat("the long report goes on and on ", 12_000) + "zanzibar"
	return map[string]string{
		"text@x": eml("text@x", "Bob <bob@example.com>", "alice@example.com", "plain quokka", date, "the wombat numbers are in, ok?\r\nUnsubscribe here"),
		"html@x": "Message-ID: <html@x>\r\nFrom: Carol <carol@example.com>\r\nTo: alice@example.com\r\nSubject: html only\r\nDate: " + date +
			"\r\nMIME-Version: 1.0\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<p>Hello <b>marmalade</b> world <a href=\"https://example.com/x\">link</a></p>\r\n",
		"both@x": "Message-ID: <both@x>\r\nFrom: dan@example.com\r\nTo: alice@example.com\r\nSubject: both kinds\r\nDate: " + date +
			"\r\nMIME-Version: 1.0\r\nContent-Type: multipart/alternative; boundary=AA\r\n\r\n--AA\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nkiwi fruit salad\r\n" +
			"--AA\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<p>kiwi <i>fruit</i> salad</p>\r\n--AA--\r\n",
		"empty@x": "Message-ID: <empty@x>\r\nFrom: erin@example.com\r\nTo: alice@example.com\r\nSubject: nothing inside\r\nDate: " + date + "\r\n\r\n",
		"big@x":   eml("big@x", "frank@example.com", "alice@example.com", "very large", date, big),
		"cid@x": "Message-ID: <cid@x>\r\nFrom: gina@example.com\r\nTo: alice@example.com\r\nSubject: inline image\r\nDate: " + date +
			"\r\nMIME-Version: 1.0\r\nContent-Type: multipart/related; boundary=RR\r\n\r\n--RR\r\nContent-Type: text/html; charset=utf-8\r\n\r\n" +
			"<p>pelican <img src=\"cid:img1@x\"></p>\r\n--RR\r\nContent-Type: image/png\r\nContent-ID: <img1@x>\r\nContent-Transfer-Encoding: base64\r\n\r\n" +
			"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==\r\n--RR--\r\n",
	}
}

// bodyReads is what every reader of a body returns for ids, and what
// searches and rules over the bodies find.
func bodyReads(t *testing.T, s *Service, acc int64, ids map[string]int64) map[string]any {
	t.Helper()
	out := map[string]any{}
	for mid, id := range ids {
		m, err := s.GetMessage(bg, id, false)
		require.NoError(t, err)
		out["GetMessage "+mid] = m
		lazy, err := s.GetMessageLazyImages(bg, id, false)
		require.NoError(t, err)
		out["GetMessageLazyImages "+mid] = lazy
		text, err := s.GetMessageText(bg, id) // MCP get_message, Wit
		require.NoError(t, err)
		out["GetMessageText "+mid] = text
		html, err := s.StoredHTML(bg, id)
		require.NoError(t, err)
		out["StoredHTML "+mid] = html
		model, err := s.mailForModel(bg, id)
		require.NoError(t, err)
		out["mailForModel "+mid] = model
		refs, err := s.inlineImageRefs(bg, id)
		require.NoError(t, err)
		out["inlineImageRefs "+mid] = refs
		src, err := s.sourceMessage(bg, acc, id) // reply and forward quoting
		require.NoError(t, err)
		out["sourceMessage "+mid] = src
		rm, ok, err := s.ruleMail(bg, id)
		require.NoError(t, err)
		rm.now = 0
		out["ruleMail "+mid] = fmt.Sprint(rm, ok)
		tags, err := s.builtinTags(bg, id)
		require.NoError(t, err)
		out["builtinTags "+mid] = tags
		_, _, members, err := s.mailTarget(bg, "message", id) // the AI cache's content hash
		require.NoError(t, err)
		out["mailTarget "+mid] = members
		for _, mode := range []string{"reply", "forward"} {
			d, err := s.NewDraft(bg, acc, mode, id)
			require.NoError(t, err)
			out["NewDraft "+mode+" "+mid] = []any{d.Subject, d.HTML, d.To, d.Cc}
			require.NoError(t, s.DeleteDraft(bg, d.ID))
		}
	}
	for _, q := range []string{"wombat", "marmalade", "kiwi", "quokka", "salad", "zanzibar", "bob@example", "ok", "ki", "é"} {
		found, err := s.Search(bg, acc, q, 50)
		require.NoError(t, err)
		var got []int64
		for _, m := range found {
			got = append(got, m.ID)
		}
		out["Search "+q] = got
	}
	for _, c := range []RuleCondition{
		{Field: FieldBody, Op: OpContains, Value: "wombat"},
		{Field: FieldBody, Op: OpContains, Value: "ok"},
		{Field: FieldBody, Op: OpNotContains, Value: "kiwi"},
		{Field: FieldBody, Op: OpIs, Value: "kiwi fruit salad"},
	} {
		p, err := s.PreviewRule(bg, RuleDoc{Trigger: TriggerReceive, Match: MatchAll, Conditions: []RuleCondition{c}}, 50)
		require.NoError(t, err)
		var got []int64
		for _, m := range p.Messages {
			got = append(got, m.ID)
		}
		out["PreviewRule "+c.Op+" "+c.Value] = []any{p.Count, got}
	}
	codes, err := s.RecentSignInCodes(bg, time.Now())
	require.NoError(t, err)
	out["RecentSignInCodes"] = codes
	return out
}

// Synced mail of every body shape reads the same with its bodies inline
// and moved, before and after RebuildSearchIndex, and the sync stores
// new mail straight in message_bodies.
func TestBodiesSameInBothLayouts(t *testing.T) {
	e := newSyncEnv(t)
	date := time.Now().Add(-time.Hour).Format(time.RFC1123Z)
	ids := map[string]int64{}
	for mid, raw := range bodyMails(date) {
		e.put(t, "INBOX", raw)
		ids[mid] = 0
	}
	e.sync(t)
	for mid := range ids {
		ids[mid] = e.idOf(t, mid)
	}
	waitBodiesMoved(t, e.s)
	both, inline, stored := bodyPlaces(t, e.s)
	require.Zero(t, both)
	require.Zero(t, inline, "the sync writes no body inline")
	require.Equal(t, len(ids), stored, "every message has a body row, an empty one too")

	moved := bodyReads(t, e.s, e.acc.ID, ids)
	assert.Contains(t, moved["Search zanzibar"], ids["big@x"], "the body of a large message is indexed")
	assert.Contains(t, moved["Search wombat"], ids["text@x"])
	assert.NotEmpty(t, moved["Search ok"])

	inlineLayout(t, e.s)
	ftsIntegrity(t, e.s)
	assert.Equal(t, moved, bodyReads(t, e.s, e.acc.ID, ids), "bodies inline")
	require.NoError(t, e.s.RebuildSearchIndex(bg))
	ftsIntegrity(t, e.s)
	assert.Equal(t, moved, bodyReads(t, e.s, e.acc.ID, ids), "bodies inline, index rebuilt")

	movedLayout(t, e.s)
	ftsIntegrity(t, e.s)
	assert.Equal(t, moved, bodyReads(t, e.s, e.acc.ID, ids), "bodies moved")
	require.NoError(t, e.s.RebuildSearchIndex(bg))
	ftsIntegrity(t, e.s)
	assert.Equal(t, moved, bodyReads(t, e.s, e.acc.ID, ids), "bodies moved, index rebuilt")

}

// A local archive's import stores its bodies in message_bodies, reads the
// same with them inline, and deleting the mail deletes them.
func TestBodiesLocalImport(t *testing.T) {
	e := newSyncEnv(t)
	acc, err := e.s.AddLocalArchive(bg, "old mail")
	require.NoError(t, err)
	folders, err := e.s.ListFolders(bg, acc.ID)
	require.NoError(t, err)
	var inbox int64
	for _, f := range folders {
		if f.Role == "inbox" {
			inbox = f.ID
		}
	}
	require.NotZero(t, inbox)
	dir := t.TempDir()
	var paths []string
	date := time.Now().Add(-time.Hour).Format(time.RFC1123Z)
	for mid, raw := range bodyMails(date) {
		p := filepath.Join(dir, strings.TrimSuffix(mid, "@x")+".eml")
		require.NoError(t, os.WriteFile(p, []byte(raw), 0o600))
		paths = append(paths, p)
	}
	sum, err := e.s.ImportEML(bg, acc.ID, inbox, paths)
	require.NoError(t, err)
	require.Equal(t, len(paths), sum.Imported, sum.Failed)
	waitBodiesMoved(t, e.s)
	ids := map[string]int64{}
	for mid := range bodyMails(date) {
		ids[mid] = e.idOf(t, mid)
	}
	_, inline, stored := bodyPlaces(t, e.s)
	require.Zero(t, inline)
	require.Equal(t, len(ids), stored)
	read := func() map[string]any {
		out := map[string]any{}
		for mid, id := range ids {
			m, err := e.s.GetMessage(bg, id, false)
			require.NoError(t, err)
			text, err := e.s.GetMessageText(bg, id)
			require.NoError(t, err)
			out[mid] = []any{m, text}
		}
		for _, q := range []string{"wombat", "marmalade", "ok", "zanzibar"} {
			found, err := e.s.Search(bg, acc.ID, q, 50)
			require.NoError(t, err)
			out["Search "+q] = len(found)
		}
		return out
	}
	moved := read()
	inlineLayout(t, e.s)
	assert.Equal(t, moved, read())
	movedLayout(t, e.s)
	assert.Equal(t, moved, read())

	var all []int64
	for _, id := range ids {
		all = append(all, id)
	}
	require.NoError(t, e.s.Delete(bg, all))
	require.NoError(t, e.s.DeleteForever(bg, all))
	require.Eventually(t, func() bool {
		return e.count(t, "SELECT count(*) FROM messages WHERE account_id = ?", acc.ID) == 0
	}, 10*time.Second, 10*time.Millisecond)
	assert.Zero(t, e.count(t, "SELECT count(*) FROM message_bodies"), "DeleteLocal takes the bodies")
	found, err := e.s.Search(bg, acc.ID, "wombat", 10)
	require.NoError(t, err)
	assert.Empty(t, found)
	ftsIntegrity(t, e.s)
}

// 回复状态 drops mass mail by the words in its body (dropMassMail):
// conversations, reply dates and the deadline checks are the same with the
// bodies inline and moved.
func TestBodiesReplyStatusSame(t *testing.T) {
	for _, seed := range []uint64{1, 2} {
		mb := newReplyMailbox(t, seed)
		s := mb.s
		read := func() []any {
			now := s.userNow(bg).Unix()
			all, err := s.replyStatuses(bg, now)
			require.NoError(t, err)
			var dl []ReaderDeadline
			for _, tid := range mb.threads {
				d, err := s.ThreadDeadline(bg, tid)
				require.NoError(t, err)
				dl = append(dl, d)
			}
			return []any{all, dl}
		}
		waitBodiesMoved(t, s)
		moved := read()
		inlineLayout(t, s)
		assert.Equal(t, moved, read(), "seed %d inline", seed)
		movedLayout(t, s)
		assert.Equal(t, moved, read(), "seed %d moved", seed)
	}
}

// A Gmail upgrade merges the rows of one server message into one: the
// kept row keeps its body, and the dropped rows' bodies go with them.
func TestBodiesGmailMerge(t *testing.T) {
	e := newSyncEnv(t, testmail.Gmail())
	require.NoError(t, e.im.User.Create("Work", nil))
	e.im.AddGmail(msg("a@x"), nil, "INBOX", "Work", testmail.GmailAllMail)
	e.sync(t)
	id := e.idOf(t, "a@x")
	before, err := e.s.GetMessageText(bg, id)
	require.NoError(t, err)
	rows := e.oldRows(t, id)
	require.Len(t, rows, 3)
	require.Equal(t, 3, e.count(t, "SELECT count(*) FROM message_bodies"), "the copies' bodies, moved by the safety trigger")
	e.sync(t)
	require.Equal(t, 1, e.count(t, "SELECT count(*) FROM messages"))
	kept := store.Int(e.rows(t, "SELECT id FROM messages")[0][0])
	after, err := e.s.GetMessageText(bg, kept)
	require.NoError(t, err)
	assert.Equal(t, before.Text, after.Text)
	assert.NotEmpty(t, after.Text)
	assert.Equal(t, 1, e.count(t, "SELECT count(*) FROM message_bodies"))
}
