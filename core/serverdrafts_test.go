package core

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/godwit-email/godwit/core/events"
	"github.com/godwit-email/godwit/core/internal/testmail"
	"github.com/godwit-email/godwit/core/mail"
	"github.com/godwit-email/godwit/core/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newServerDraftsEnv is a send env whose server has a Drafts folder, synced
// once, with ServerDrafts set to on.
func newServerDraftsEnv(t *testing.T, on bool, opts ...testmail.Option) *sendEnv {
	t.Helper()
	e := newSendEnv(t, false, opts...)
	require.NoError(t, e.im.User.Create("Drafts", nil))
	if on {
		_, err := e.s.UpdateAccount(bg, e.acc.ID, AccountPatch{ServerDrafts: &on})
		require.NoError(t, err)
	}
	e.sync(t)
	return e
}

// newDraftTo saves a new draft to bob with subject and body text.
func (e *sendEnv) newDraftTo(t *testing.T, subject, text string) Draft {
	t.Helper()
	d, err := e.s.NewDraft(bg, e.acc.ID, "new", 0)
	require.NoError(t, err)
	d.To, d.Bcc, d.Subject, d.HTML = []string{"Bob <bob@example.com>"}, []string{"dave@example.com"}, subject, "<p>"+text+"</p>"
	d, err = e.s.SaveDraft(bg, d)
	require.NoError(t, err)
	return d
}

// idle lets ServerDraftIdle pass and syncs. A draft's updated_at is wall
// clock seconds, so the Service's clock moves past them, never back.
func (e *sendEnv) idle(t *testing.T) {
	t.Helper()
	e.clock.Advance(max(time.Now().Add(ServerDraftIdle+time.Second).Sub(e.clock.Now()), time.Second))
	e.sync(t)
}

// rawOnServer is every message in folder, as the server holds it, by UID.
func (e *sendEnv) rawOnServer(t *testing.T, folder string) [][]byte {
	t.Helper()
	c := e.dial(t)
	sel, err := c.Select(folder, &imap.SelectOptions{ReadOnly: true}).Wait()
	require.NoError(t, err)
	if sel.NumMessages == 0 {
		return nil
	}
	var all imap.UIDSet
	all.AddRange(1, 0)
	sec := &imap.FetchItemBodySection{Peek: true}
	msgs, err := c.Fetch(all, &imap.FetchOptions{UID: true, BodySection: []*imap.FetchItemBodySection{sec}}).Collect()
	require.NoError(t, err)
	slices.SortFunc(msgs, func(a, b *imapclient.FetchMessageBuffer) int { return int(a.UID) - int(b.UID) })
	out := make([][]byte, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.FindBodySection(sec))
	}
	return out
}

// listedMID counts the account's messages with Message-ID mid that the lists show.
func (e *sendEnv) listedMID(t *testing.T, mid string) int {
	t.Helper()
	return e.count(t, "SELECT count(*) FROM messages m WHERE m.message_id = ? AND "+notPurgedCond, mid)
}

func parsed(t *testing.T, raw []byte) mail.Parsed {
	t.Helper()
	p, err := mail.Parse(raw)
	require.NoError(t, err)
	return *p
}

func TestServerDraftStoredAfterIdleAndReplaced(t *testing.T) {
	e := newServerDraftsEnv(t, true)
	d := e.newDraftTo(t, "plan", "first text")
	e.sync(t)
	assert.Empty(t, e.onServer(t, "Drafts"), "not before ServerDraftIdle")

	e.idle(t)
	on := e.onServer(t, "Drafts")
	require.Len(t, on, 1)
	assert.Contains(t, on[0], `\Draft`)
	assert.Contains(t, on[0], `\Seen`)
	raw := e.rawOnServer(t, "Drafts")[0]
	first := parsed(t, raw)
	assert.Equal(t, "plan", first.Subject)
	assert.Contains(t, first.HTML, "first text")
	assert.Contains(t, headerValue(raw, "Bcc"), "dave@example.com", "a draft keeps its Bcc")
	token := headerValue(raw, DraftIDHeader)
	assert.NotEmpty(t, token)

	e.sync(t) // the copy reaches local storage, hidden from the lists
	assert.Equal(t, 1, e.count(t, "SELECT count(*) FROM messages WHERE message_id = ?", first.MessageID))
	assert.Zero(t, e.listedMID(t, first.MessageID))

	d.HTML = "<p>second text</p>"
	_, err := e.s.SaveDraft(bg, d)
	require.NoError(t, err)
	e.idle(t)
	raws := e.rawOnServer(t, "Drafts")
	require.Len(t, raws, 1, "the new copy replaced the old one")
	second := parsed(t, raws[0])
	assert.NotEqual(t, first.MessageID, second.MessageID)
	assert.Contains(t, second.HTML, "second text")
	assert.Equal(t, token, headerValue(raws[0], DraftIDHeader), "one ID across the draft's copies")

	e.sync(t)
	e.sync(t)
	assert.Zero(t, e.count(t, "SELECT count(*) FROM server_drafts WHERE state = 'removed'"), "forgotten once the sync dropped it")
	assert.Equal(t, 1, e.count(t, "SELECT count(*) FROM server_drafts"))
}

func TestServerDraftFlushedOnClose(t *testing.T) {
	e := newServerDraftsEnv(t, true)
	d := e.newDraftTo(t, "plan", "text")
	require.NoError(t, e.s.FlushServerDraft(bg, d.ID))
	e.sync(t)
	assert.Len(t, e.onServer(t, "Drafts"), 1, "stored without waiting")
}

func TestServerDraftOffStaysLocal(t *testing.T) {
	e := newServerDraftsEnv(t, false)
	d := e.newDraftTo(t, "plan", "text")
	require.NoError(t, e.s.FlushServerDraft(bg, d.ID))
	e.idle(t)
	assert.Empty(t, e.onServer(t, "Drafts"))
	assert.Zero(t, e.count(t, "SELECT count(*) FROM server_drafts"))
}

func TestServerDraftRemovedWhenSent(t *testing.T) {
	e := newServerDraftsEnv(t, true)
	d := e.newDraftTo(t, "plan", "text")
	e.idle(t)
	require.Len(t, e.onServer(t, "Drafts"), 1)
	require.NoError(t, e.s.Send(bg, d.ID))
	e.sync(t)
	assert.Len(t, e.onServer(t, "Drafts"), 1, "queued: the copy stays while it can be taken back")
	e.clock.Advance(UndoSendWindow)
	e.sync(t)
	assert.Len(t, e.smtp.Messages(), 1)
	assert.Empty(t, e.onServer(t, "Drafts"), "sent: the copy goes")
	sent := e.rawOnServer(t, "Sent")
	require.Len(t, sent, 1)
	assert.NotContains(t, string(sent[0]), DraftIDHeader, "the sent message carries no draft fields")
	assert.NotContains(t, string(sent[0]), "Bcc:")
}

func TestServerDraftRemovedWhenDeleted(t *testing.T) {
	e := newServerDraftsEnv(t, true)
	d := e.newDraftTo(t, "plan", "text")
	e.idle(t)
	require.Len(t, e.onServer(t, "Drafts"), 1)
	require.NoError(t, e.s.DeleteDraft(bg, d.ID))
	e.sync(t)
	assert.Empty(t, e.onServer(t, "Drafts"))
}

func TestServerDraftReplyKeepsLinkInGodwitHeaders(t *testing.T) {
	e := newServerDraftsEnv(t, true)
	e.put(t, "INBOX", eml("o@x", "Bob <bob@example.com>", "alice@example.com", "plan", day, "the plan"))
	e.sync(t)
	d, err := e.s.NewDraft(bg, e.acc.ID, "reply_all", e.idOf(t, "o@x"))
	require.NoError(t, err)
	require.NoError(t, e.s.FlushServerDraft(bg, d.ID))
	e.sync(t)
	raws := e.rawOnServer(t, "Drafts")
	require.Len(t, raws, 1)
	p := parsed(t, raws[0])
	assert.Equal(t, "o@x", p.InReplyTo)
	assert.Equal(t, "<o@x>", headerValue(raws[0], DraftSourceHeader))
	assert.Equal(t, "reply_all", headerValue(raws[0], DraftModeHeader))
}

func TestServerDraftWithoutUIDPlus(t *testing.T) {
	e := newServerDraftsEnv(t, true, testmail.WithoutUIDPlus())
	d := e.newDraftTo(t, "plan", "first")
	e.idle(t)
	require.Len(t, e.onServer(t, "Drafts"), 1)
	assert.Equal(t, 1, e.count(t, "SELECT count(*) FROM server_drafts WHERE state = 'stored' AND server_ref IS NOT NULL"),
		"no APPENDUID: found by its Message-ID")

	d.HTML = "<p>second</p>"
	_, err := e.s.SaveDraft(bg, d)
	require.NoError(t, err)
	e.idle(t)
	on := e.onServer(t, "Drafts")
	require.Len(t, on, 2, "no UID EXPUNGE without UIDPLUS: a plain EXPUNGE would remove what other apps flagged")
	assert.Contains(t, on[0], `\Deleted`, "the old copy is flagged for the next EXPUNGE")
	assert.NotContains(t, on[1], `\Deleted`)

	e.sync(t)
	for _, r := range e.rawOnServer(t, "Drafts") {
		assert.Zero(t, e.listedMID(t, parsed(t, r).MessageID), "neither copy shows in the lists")
	}
	require.NoError(t, e.s.DeleteDraft(bg, d.ID))
	e.sync(t)
	for _, f := range e.onServer(t, "Drafts") {
		assert.Contains(t, f, `\Deleted`)
	}
}

// foreignDraftEml is a draft another app saved: a reply to o@x with a Bcc and
// an attachment, and extra header lines.
func foreignDraftEml(id, subject, date string, hdr ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Message-ID: <%s>\r\nFrom: alice@example.com\r\nTo: Bob <bob@example.com>\r\nBcc: dave@example.com\r\nSubject: %s\r\nDate: %s\r\n", id, subject, date)
	for _, l := range hdr {
		b.WriteString(l + "\r\n")
	}
	b.WriteString("MIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=XX\r\n\r\n")
	b.WriteString("--XX\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<p>their <b>words</b></p>\r\n")
	b.WriteString("--XX\r\nContent-Type: application/pdf\r\nContent-Disposition: attachment; filename=\"plan.pdf\"\r\nContent-Transfer-Encoding: base64\r\n\r\nJVBERi0xLjQK\r\n")
	b.WriteString("--XX--\r\n")
	return b.String()
}

func TestOpenServerDraftReply(t *testing.T) {
	e := newServerDraftsEnv(t, false)
	e.put(t, "INBOX", eml("o@x", "Bob <bob@example.com>", "alice@example.com", "plan", day, "the plan"))
	e.put(t, "Drafts", foreignDraftEml("their@x", "Re: plan", day, "In-Reply-To: <o@x>", "References: <o@x>"))
	e.sync(t)
	msg := e.idOf(t, "their@x")
	require.Equal(t, 1, e.listedMID(t, "their@x"), "another app's draft shows as a message")

	id, err := e.s.OpenServerDraft(bg, msg)
	require.NoError(t, err)
	d, err := e.s.GetDraft(bg, id)
	require.NoError(t, err)
	assert.Equal(t, "reply", d.Mode)
	assert.Equal(t, e.idOf(t, "o@x"), d.SourceMessageID)
	assert.Equal(t, []string{"Bob <bob@example.com>"}, d.To)
	assert.Equal(t, []string{"dave@example.com"}, d.Bcc)
	assert.Equal(t, "Re: plan", d.Subject)
	assert.Contains(t, d.HTML, "<b>words</b>")
	require.Len(t, d.Attachments, 1)
	assert.Equal(t, "plan.pdf", d.Attachments[0].Filename)
	assert.False(t, d.FromProgram, "the interface's own take-over")
	assert.Zero(t, e.listedMID(t, "their@x"), "the message is now the draft's copy")

	again, err := e.s.OpenServerDraft(bg, msg)
	require.NoError(t, err)
	assert.Equal(t, id, again, "opened once")

	require.NoError(t, e.s.Send(bg, id))
	e.clock.Advance(UndoSendWindow)
	e.sync(t)
	got := e.smtp.Messages()
	require.Len(t, got, 1)
	p := parsed(t, got[0].Data)
	assert.Equal(t, "o@x", p.InReplyTo)
	assert.Empty(t, e.onServer(t, "Drafts"), "sent: the other app's draft goes")
}

func TestOpenServerDraftExchangeDroppedInReplyTo(t *testing.T) {
	e := newServerDraftsEnv(t, false)
	e.put(t, "INBOX", eml("o@x", "Bob <bob@example.com>", "alice@example.com", "plan", day, "the plan"))
	e.put(t, "Drafts", foreignDraftEml("their@x", "Re: plan", day, DraftSourceHeader+": <o@x>", DraftModeHeader+": reply_all"))
	e.sync(t)
	id, err := e.s.OpenServerDraft(bg, e.idOf(t, "their@x"))
	require.NoError(t, err)
	d, err := e.s.GetDraft(bg, id)
	require.NoError(t, err)
	assert.Equal(t, "reply_all", d.Mode)
	assert.Equal(t, e.idOf(t, "o@x"), d.SourceMessageID)
}

func TestOpenServerDraftPlainTextAndNoSource(t *testing.T) {
	e := newServerDraftsEnv(t, false)
	e.put(t, "Drafts", eml("their@x", "alice@example.com", "bob@example.com", "notes", day, "line one\nline <two>", "In-Reply-To: <unknown@x>"))
	e.sync(t)
	id, err := e.s.OpenServerDraft(bg, e.idOf(t, "their@x"))
	require.NoError(t, err)
	d, err := e.s.GetDraft(bg, id)
	require.NoError(t, err)
	assert.Equal(t, "new", d.Mode, "the original is not here")
	assert.Zero(t, d.SourceMessageID)
	assert.Contains(t, d.HTML, "line one")
	assert.Contains(t, d.HTML, "&lt;two&gt;")
}

func TestOpenServerDraftRefusesOtherMail(t *testing.T) {
	e := newServerDraftsEnv(t, false)
	e.put(t, "INBOX", eml("o@x", "Bob <bob@example.com>", "alice@example.com", "plan", day, "the plan"))
	e.sync(t)
	_, err := e.s.OpenServerDraft(bg, e.idOf(t, "o@x"))
	assert.ErrorIs(t, err, ErrNotServerDraft)
}

func TestOpenServerDraftReplacedWhenStored(t *testing.T) {
	e := newServerDraftsEnv(t, true)
	e.put(t, "Drafts", foreignDraftEml("their@x", "notes", day))
	e.sync(t)
	id, err := e.s.OpenServerDraft(bg, e.idOf(t, "their@x"))
	require.NoError(t, err)
	e.idle(t)
	require.Len(t, e.onServer(t, "Drafts"), 1, "nothing to store: the server holds this revision")

	d, err := e.s.GetDraft(bg, id)
	require.NoError(t, err)
	d.Subject = "notes, edited"
	_, err = e.s.SaveDraft(bg, d)
	require.NoError(t, err)
	e.idle(t)
	raws := e.rawOnServer(t, "Drafts")
	require.Len(t, raws, 1, "Godwit's copy replaced the other app's")
	assert.Equal(t, "notes, edited", parsed(t, raws[0]).Subject)
}

// seenCopy stores a fresh draft and syncs until local storage holds its
// copy; it returns the draft and its X-Godwit-Draft-ID.
func (e *sendEnv) seenCopy(t *testing.T) (Draft, string) {
	t.Helper()
	d := e.newDraftTo(t, "mine", "my text")
	e.idle(t)
	e.sync(t)
	require.Equal(t, 1, e.count(t, "SELECT count(*) FROM server_drafts WHERE seen = 1"))
	return d, store.Str(e.rows(t, "SELECT token FROM server_drafts")[0][0])
}

func TestServerDraftChangedElsewhereServerNewer(t *testing.T) {
	e := newServerDraftsEnv(t, true)
	d, token := e.seenCopy(t)
	ch, cancel := e.s.bus.Subscribe()
	defer cancel()
	e.expungeAll(t, "Drafts") // another app saves its edit as a new message
	e.put(t, "Drafts", foreignDraftEml("their@x", "theirs", time.Now().Add(time.Hour).Format(time.RFC1123Z), DraftIDHeader+": "+token))
	e.sync(t)

	got, err := e.s.GetDraft(bg, d.ID)
	require.NoError(t, err)
	assert.Equal(t, "theirs", got.Subject, "the server's version was saved last")
	assert.Len(t, got.Attachments, 1)
	assert.Greater(t, got.Revision, d.Revision)
	drafts, err := e.s.ListDrafts(bg, e.acc.ID)
	require.NoError(t, err)
	require.Len(t, drafts, 2, "the local version is kept as a draft of its own")
	assert.True(t, slices.ContainsFunc(drafts, func(x Draft) bool { return x.Subject == "mine" && strings.Contains(x.HTML, "my text") }))
	evs := ofType(collect(ch), events.DraftConflict)
	require.Len(t, evs, 1)
	assert.Equal(t, "server_newer", evs[0].Kind)
	assert.Zero(t, e.listedMID(t, "their@x"))

	e.idle(t) // the kept version is stored on its own; theirs is the draft's copy
	assert.Len(t, e.onServer(t, "Drafts"), 2)
}

func TestServerDraftChangedElsewhereLocalNewer(t *testing.T) {
	e := newServerDraftsEnv(t, true)
	d, token := e.seenCopy(t)
	ch, cancel := e.s.bus.Subscribe()
	defer cancel()
	e.expungeAll(t, "Drafts")
	e.put(t, "Drafts", foreignDraftEml("their@x", "theirs", time.Now().Add(-time.Hour).Format(time.RFC1123Z), DraftIDHeader+": "+token))
	e.sync(t)

	got, err := e.s.GetDraft(bg, d.ID)
	require.NoError(t, err)
	assert.Equal(t, "mine", got.Subject, "the local version was saved last")
	drafts, err := e.s.ListDrafts(bg, e.acc.ID)
	require.NoError(t, err)
	require.Len(t, drafts, 2, "the server's version is kept as a draft of its own")
	assert.True(t, slices.ContainsFunc(drafts, func(x Draft) bool { return x.Subject == "theirs" && len(x.Attachments) == 1 }))
	evs := ofType(collect(ch), events.DraftConflict)
	require.Len(t, evs, 1)
	assert.Equal(t, "local_newer", evs[0].Kind)
	var subjects []string
	for _, r := range e.rawOnServer(t, "Drafts") {
		p := parsed(t, r)
		assert.NotEqual(t, "their@x", p.MessageID, "the local version replaced theirs at once")
		if p.Subject == "mine" {
			assert.Equal(t, token, headerValue(r, DraftIDHeader))
		}
		subjects = append(subjects, p.Subject)
	}
	assert.ElementsMatch(t, []string{"mine", "theirs"}, subjects, "the draft, and their version as a draft of its own")
}

func TestServerDraftRemovedElsewhere(t *testing.T) {
	e := newServerDraftsEnv(t, true)
	d, _ := e.seenCopy(t)
	ch, cancel := e.s.bus.Subscribe()
	defer cancel()
	e.expungeAll(t, "Drafts") // sent or deleted on the phone
	e.sync(t)
	_, err := e.s.GetDraft(bg, d.ID)
	require.NoError(t, err, "the draft stays")
	evs := ofType(collect(ch), events.DraftConflict)
	require.Len(t, evs, 1)
	assert.Equal(t, "server_gone", evs[0].Kind)
	e.idle(t)
	assert.Empty(t, e.onServer(t, "Drafts"), "not stored again until it changes")

	d.Subject = "changed"
	_, err = e.s.SaveDraft(bg, d)
	require.NoError(t, err)
	e.idle(t)
	assert.Len(t, e.onServer(t, "Drafts"), 1)
}

// A draft's own header fields and read receipt go to its server copy, and
// the receipt comes back from a copy another app changed.
func TestServerDraftKeepsHeaderFields(t *testing.T) {
	e := newServerDraftsEnv(t, true)
	d := e.newDraftTo(t, "plan", "text")
	d.Headers, d.ReadReceipt = []mail.Header{{Name: "X-Tracking-ID", Value: "42"}}, true
	d, err := e.s.SaveDraft(bg, d)
	require.NoError(t, err)
	require.NoError(t, e.s.FlushServerDraft(bg, d.ID))
	e.sync(t)
	raws := e.rawOnServer(t, "Drafts")
	require.Len(t, raws, 1)
	assert.Equal(t, "42", headerValue(raws[0], "X-Tracking-ID"))
	assert.Contains(t, headerValue(raws[0], "Disposition-Notification-To"), e.acc.Email)

	p, _, err := e.s.parseServerDraft(bg, e.acc.ID, raws[0])
	require.NoError(t, err)
	assert.True(t, p.ReadReceipt)
	assert.Nil(t, p.Headers, "another app's fields are never taken")
}
