package core

import (
	"bytes"
	"errors"
	netmail "net/mail"
	"os"
	"testing"

	"github.com/godwit-email/godwit/core/mail"
	"github.com/godwit-email/godwit/core/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A draft's own header fields are stored in order, kept by a save that
// leaves them nil (the compose window's), replaced by one that gives them,
// and sent after Godwit's.
func TestDraftHeadersSavedAndSent(t *testing.T) {
	e := newSendEnv(t, false)
	d, err := e.s.NewDraft(bg, e.acc.ID, "new", 0)
	require.NoError(t, err)
	assert.Nil(t, d.Headers)
	d.To, d.Subject, d.HTML = []string{"bob@example.com"}, "hello", "<p>hi</p>"
	d.Headers = []mail.Header{{Name: "x-hermes-session", Value: "abc"}, {Name: "X-Note", Value: "café"}}
	d, err = e.s.SaveDraft(bg, d)
	require.NoError(t, err)
	want := []mail.Header{{Name: "X-Hermes-Session", Value: "abc"}, {Name: "X-Note", Value: "café"}}
	assert.Equal(t, want, d.Headers)

	rev := d.Revision
	d.Headers, d.Subject = nil, "hello again" // as the compose window saves
	d, err = e.s.SaveDraft(bg, d)
	require.NoError(t, err)
	assert.Equal(t, want, d.Headers, "nil keeps them")
	assert.Equal(t, rev+1, d.Revision)

	d.Headers = []mail.Header{{Name: "X-Hermes-Session", Value: "def"}}
	d, err = e.s.SaveDraft(bg, d)
	require.NoError(t, err)
	assert.Equal(t, []mail.Header{{Name: "X-Hermes-Session", Value: "def"}}, d.Headers, "a list replaces them")
	assert.Equal(t, rev+2, d.Revision, "a header change is a new revision: an approval of the old one fails")

	require.NoError(t, e.s.Send(bg, d.ID))
	raw, err := os.ReadFile(store.Str(e.rows(t, "SELECT raw_path FROM outbox")[0][0]))
	require.NoError(t, err)
	assert.Contains(t, string(raw), "\r\nX-Hermes-Session: def\r\n", "the outbox item carries them")
	e.clock.Advance(UndoSendWindow)
	e.sync(t)
	got := e.smtp.Messages()
	require.Len(t, got, 1)
	m, err := netmail.ReadMessage(bytes.NewReader(got[0].Data))
	require.NoError(t, err)
	assert.Equal(t, "def", m.Header.Get("X-Hermes-Session"))
	assert.Empty(t, m.Header.Get("X-Note"))
}

func TestDraftHeadersCleared(t *testing.T) {
	e := newSendEnv(t, false)
	d, err := e.s.SaveDraft(bg, Draft{AccountID: e.acc.ID, Mode: "new", Headers: []mail.Header{{Name: "X-A", Value: "1"}}})
	require.NoError(t, err)
	assert.Equal(t, []mail.Header{{Name: "X-A", Value: "1"}}, d.Headers, "set on insert")
	d.Headers = []mail.Header{}
	d, err = e.s.SaveDraft(bg, d)
	require.NoError(t, err)
	assert.Nil(t, d.Headers, "an empty list removes them")
	assert.Equal(t, "[]", store.Str(e.rows(t, "SELECT headers FROM drafts WHERE id = ?", d.ID)[0][0]))
}

// A field Godwit writes, or one that injects another, fails the save and
// changes nothing.
func TestDraftHeadersRefused(t *testing.T) {
	e := newSendEnv(t, false)
	d, err := e.s.NewDraft(bg, e.acc.ID, "new", 0)
	require.NoError(t, err)
	d.Headers = []mail.Header{{Name: "X-Ok", Value: "1"}}
	d, err = e.s.SaveDraft(bg, d)
	require.NoError(t, err)
	for _, h := range []mail.Header{
		{Name: "Bcc", Value: "eve@example.com"}, {Name: "Reply-To", Value: "eve@example.com"}, {Name: "Message-ID", Value: "<x@y>"},
		{Name: "X-A", Value: "1\r\nBcc: eve@example.com"}, {Name: "X-A\nBcc", Value: "eve@example.com"}, {Name: mail.RuleHeader, Value: "1"},
	} {
		bad := d
		bad.Subject, bad.Headers = "changed", []mail.Header{h}
		_, err := e.s.SaveDraft(bg, bad)
		var he *mail.HeaderError
		assert.True(t, errors.As(err, &he), "%q: %v", h, err)
		_, err = e.s.SaveDraft(bg, Draft{AccountID: e.acc.ID, Mode: "new", Headers: []mail.Header{h}})
		assert.True(t, errors.As(err, &he), "%q on insert", h)
	}
	now, err := e.s.GetDraft(bg, d.ID)
	require.NoError(t, err)
	assert.Equal(t, d.Revision, now.Revision)
	assert.Equal(t, []mail.Header{{Name: "X-Ok", Value: "1"}}, now.Headers)
	assert.Equal(t, 1, e.count(t, "SELECT count(*) FROM drafts"))
}

// A header value's placeholder is restored when sent, as the subject's is,
// and one that names no value is never mailed; a link reference likewise.
func TestDraftHeaderPlaceholders(t *testing.T) {
	const card = "4111 1111 1111 1111"
	e := newSendEnv(t, false)
	m, err := e.s.masker()
	require.NoError(t, err)
	p, err := m.Text(card)
	require.NoError(t, err)
	d, err := e.s.NewDraft(bg, e.acc.ID, "new", 0)
	require.NoError(t, err)
	d.To, d.HTML = []string{"bob@example.com"}, "<p>hi</p>"
	d.Headers = []mail.Header{{Name: "X-Card", Value: "card " + p}}
	d, err = e.s.SaveDraft(bg, d)
	require.NoError(t, err)
	shown := d
	require.NoError(t, e.s.expandDraft(bg, &shown))
	assert.Equal(t, "card "+card, shown.Headers[0].Value)
	assert.Equal(t, "card "+p, d.Headers[0].Value, "expanding a copy leaves the draft's slice alone")
	require.NoError(t, e.s.Send(bg, d.ID))
	raw, err := os.ReadFile(store.Str(e.rows(t, "SELECT raw_path FROM outbox")[0][0]))
	require.NoError(t, err)
	assert.Contains(t, string(raw), "X-Card: card "+card)

	for name, v := range map[string]string{"placeholder": "{{BANK_CARD_aaaaaaaaaaaa}}", "link": linkRefPrefix + "zoom.us/j?m=999&h=0000000000000000"} {
		d, err := e.s.NewDraft(bg, e.acc.ID, "new", 0)
		require.NoError(t, err)
		d.To, d.HTML, d.Headers = []string{"bob@example.com"}, "<p>hi</p>", []mail.Header{{Name: "X-V", Value: v}}
		d, err = e.s.SaveDraft(bg, d)
		require.NoError(t, err)
		err = e.s.Send(bg, d.ID)
		assert.True(t, errors.Is(err, ErrPlaceholder) || errors.Is(err, ErrLinkRef), "%s: %v", name, err)
	}
	assert.Equal(t, 1, e.count(t, "SELECT count(*) FROM outbox"))
}

// A mailto: link's X- fields reach its draft; a link reference planted in
// one is defused as the subject's is.
func TestMailtoDraftHeaders(t *testing.T) {
	e := newSendEnv(t, false)
	d, err := e.s.NewMailtoDraft(bg, e.acc.ID, "mailto:bob@example.com?subject=Hi&X-Hermes-Session=abc&utm_source=x&Bcc=eve@example.com&X-Ref="+linkRefPrefix+"1/x")
	require.NoError(t, err)
	assert.Equal(t, []string{"bob@example.com"}, d.To)
	require.Len(t, d.Headers, 2)
	assert.Equal(t, mail.Header{Name: "X-Hermes-Session", Value: "abc"}, d.Headers[0])
	assert.Equal(t, "X-Ref", d.Headers[1].Name)
	assert.NotContains(t, d.Headers[1].Value, linkRefPrefix)
	assert.Equal(t, []string{"eve@example.com"}, d.Bcc, "bcc is the draft's Bcc, shown in its field")
}

// A read receipt is a draft field, sent as Disposition-Notification-To to
// the sending account's address; a save with false drops it.
func TestDraftReadReceipt(t *testing.T) {
	e := newSendEnv(t, false)
	d, err := e.s.NewDraft(bg, e.acc.ID, "new", 0)
	require.NoError(t, err)
	d.To, d.HTML, d.ReadReceipt = []string{"bob@example.com"}, "<p>hi</p>", true
	d, err = e.s.SaveDraft(bg, d)
	require.NoError(t, err)
	assert.True(t, d.ReadReceipt)
	d.Subject = "receipt"
	d, err = e.s.SaveDraft(bg, d)
	require.NoError(t, err)
	assert.True(t, d.ReadReceipt, "a save from GetDraft keeps it")
	require.NoError(t, e.s.Send(bg, d.ID))
	raw, err := os.ReadFile(store.Str(e.rows(t, "SELECT raw_path FROM outbox")[0][0]))
	require.NoError(t, err)
	m, err := netmail.ReadMessage(bytes.NewReader(raw))
	require.NoError(t, err)
	got, err := m.Header.AddressList("Disposition-Notification-To")
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "alice@example.com", got[0].Address)

	d2, err := e.s.SaveDraft(bg, Draft{AccountID: e.acc.ID, Mode: "new", ReadReceipt: true})
	require.NoError(t, err)
	d2.ReadReceipt = false
	d2, err = e.s.SaveDraft(bg, d2)
	require.NoError(t, err)
	assert.False(t, d2.ReadReceipt)
}
