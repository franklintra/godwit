package core

import (
	"bytes"
	netmail "net/mail"
	"testing"

	"github.com/godwit-email/godwit/core/internal/testmail"
	"github.com/godwit-email/godwit/core/mail"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIdentities(t *testing.T) {
	e := newSendEnv(t, false)
	acc := e.acc
	a, err := e.s.AddIdentity(bg, acc.ID, IdentityInput{Email: " Belcova <contact@belcova.fr> "})
	require.NoError(t, err)
	assert.Equal(t, Identity{ID: a.ID, AccountID: acc.ID, Name: "Belcova", Email: "contact@belcova.fr"}, a, "the name typed with the address")

	_, err = e.s.AddIdentity(bg, acc.ID, IdentityInput{Name: "Belcova", Email: "CONTACT@belcova.fr"})
	assert.ErrorIs(t, err, ErrIdentityExists, "the address ignores case")
	b, err := e.s.AddIdentity(bg, acc.ID, IdentityInput{Name: "Franklin at Belcova", Email: "contact@belcova.fr", ReplyTo: "team@belcova.fr", IsDefault: true})
	require.NoError(t, err, "another name on the same address")
	assert.Equal(t, "team@belcova.fr", mail.AddrKey(b.ReplyTo))

	for _, bad := range []IdentityInput{{Email: "nope"}, {Email: "a@b.example", ReplyTo: "nope"}, {Email: "a@b.example", Name: "a\nb"}} {
		_, err := e.s.AddIdentity(bg, acc.ID, bad)
		assert.ErrorIs(t, err, ErrIdentityInvalid, "%+v", bad)
	}
	_, err = e.s.AddIdentity(bg, acc.ID, IdentityInput{Email: "a@b.example", SignatureID: 999})
	assert.ErrorIs(t, err, ErrSignatureNotFound)
	_, err = e.s.AddIdentity(bg, 999, IdentityInput{Email: "a@b.example"})
	assert.ErrorIs(t, err, ErrAccountNotFound)

	// One default per account: the newest takes it.
	on := true
	_, err = e.s.UpdateIdentity(bg, a.ID, IdentityPatch{IsDefault: &on})
	require.NoError(t, err)
	ids, err := e.s.ListIdentities(bg, acc.ID)
	require.NoError(t, err)
	require.Len(t, ids, 2)
	assert.True(t, ids[0].IsDefault)
	assert.False(t, ids[1].IsDefault)

	// A new address keeps the name, unless it comes with one.
	addr := "hello@belcova.fr"
	a, err = e.s.UpdateIdentity(bg, a.ID, IdentityPatch{Email: &addr})
	require.NoError(t, err)
	assert.Equal(t, "Belcova", a.Name)
	assert.Equal(t, "hello@belcova.fr", a.Email)
	dup := "Franklin at Belcova <contact@belcova.fr>"
	_, err = e.s.UpdateIdentity(bg, a.ID, IdentityPatch{Email: &dup})
	assert.ErrorIs(t, err, ErrIdentityExists)

	// Removing it sends its drafts as the account.
	d, err := e.s.NewDraft(bg, acc.ID, "new", 0)
	require.NoError(t, err)
	assert.Equal(t, a.ID, d.IdentityID, "new mail starts from the default")
	require.NoError(t, e.s.RemoveIdentity(bg, a.ID))
	d, err = e.s.GetDraft(bg, d.ID)
	require.NoError(t, err)
	assert.Zero(t, d.IdentityID)
	assertFrom(t, d.From, "Alice", "alice@example.com")
	assert.ErrorIs(t, e.s.RemoveIdentity(bg, a.ID), ErrIdentityNotFound)
	all, err := e.s.ListIdentities(bg, 0)
	require.NoError(t, err)
	assert.Equal(t, []int64{b.ID}, []int64{all[0].ID})

	// A local archive never sends, so it has none.
	arch, err := e.s.AddLocalArchive(bg, "Old mail")
	require.NoError(t, err)
	_, err = e.s.AddIdentity(bg, arch.ID, IdentityInput{Email: "x@y.example"})
	assert.ErrorIs(t, err, ErrLocalArchive)
}

// addAccount adds another account, on servers of its own.
func (e *sendEnv) addAccount(t *testing.T, email string) Account {
	t.Helper()
	im := testmail.StartIMAP(t)
	smtpAddr, _ := testmail.StartSMTP(t)
	acc, err := e.s.AddAccount(bg, internalInput(t, im.Addr, smtpAddr, email, false))
	require.NoError(t, err)
	return acc
}

func assertFrom(t *testing.T, from, name, addr string) {
	t.Helper()
	a, err := netmail.ParseAddress(from)
	require.NoError(t, err, from)
	assert.Equal(t, name, a.Name)
	assert.Equal(t, addr, a.Address)
}

// A draft sent as an identity has the identity's From, Reply-To and
// signature; the envelope sender stays the account's address.
func TestSendAsIdentity(t *testing.T) {
	e := newSendEnv(t, false)
	sig, err := e.s.CreateSignature(bg, "Belcova")
	require.NoError(t, err)
	text := "Belcova SAS"
	_, err = e.s.UpdateSignature(bg, sig.ID, SignaturePatch{HTML: &text})
	require.NoError(t, err)
	id, err := e.s.AddIdentity(bg, e.acc.ID, IdentityInput{Name: "Belcova", Email: "contact@belcova.fr", ReplyTo: "team@belcova.fr", SignatureID: sig.ID, IsDefault: true})
	require.NoError(t, err)

	d, err := e.s.NewDraft(bg, e.acc.ID, "new", 0)
	require.NoError(t, err)
	assert.Equal(t, id.ID, d.IdentityID)
	assertFrom(t, d.From, "Belcova", "contact@belcova.fr")
	assert.Equal(t, id.ReplyTo, d.ReplyTo)
	_, _, s := mail.SplitDraft(d.HTML)
	assert.Contains(t, s, "Belcova SAS")
	assert.NotContains(t, d.HTML, "Godwit", "not the account's signature")

	// The account's own address: its signature, and the identity's Reply-To goes.
	d, err = e.s.SetDraftFrom(bg, d.ID, e.acc.ID, 0, d.Revision)
	require.NoError(t, err)
	assert.Zero(t, d.IdentityID)
	assertFrom(t, d.From, "Alice", "alice@example.com")
	assert.Empty(t, d.ReplyTo)
	_, _, s = mail.SplitDraft(d.HTML)
	assert.Contains(t, s, "Godwit")
	assert.NotContains(t, d.HTML, "Belcova SAS")

	// A Reply-To the user typed stays through a switch.
	d.To, d.Subject, d.ReplyTo, d.HTML = []string{"bob@example.com"}, "hello", "me@elsewhere.example", mail.JoinDraft("<p>hi</p>", "", s)
	d, err = e.s.SaveDraft(bg, d)
	require.NoError(t, err)
	_, err = e.s.SetDraftFrom(bg, d.ID, e.acc.ID, id.ID, d.Revision-1)
	assert.ErrorIs(t, err, ErrStaleDraft)
	d, err = e.s.SetDraftFrom(bg, d.ID, e.acc.ID, id.ID, d.Revision)
	require.NoError(t, err)
	assert.Equal(t, "me@elsewhere.example", d.ReplyTo)
	d.ReplyTo = ""
	d, err = e.s.SaveDraft(bg, d)
	require.NoError(t, err)

	// An identity of another account is refused.
	other := e.addAccount(t, "carol@example.com")
	theirs, err := e.s.AddIdentity(bg, other.ID, IdentityInput{Email: "shared@example.com"})
	require.NoError(t, err)
	_, err = e.s.SetDraftFrom(bg, d.ID, e.acc.ID, theirs.ID, d.Revision)
	assert.ErrorIs(t, err, ErrIdentityNotFound)
	// SetDraftAccount sends as the account's default identity, and back.
	d, err = e.s.SetDraftAccount(bg, d.ID, other.ID, d.Revision)
	require.NoError(t, err)
	assert.Zero(t, d.IdentityID, "carol has no default identity")
	d, err = e.s.SetDraftAccount(bg, d.ID, e.acc.ID, d.Revision)
	require.NoError(t, err)
	assert.Equal(t, id.ID, d.IdentityID)
	assert.Equal(t, "team@belcova.fr", mail.AddrKey(d.ReplyTo), "the identity's, as the draft had none")

	require.NoError(t, e.s.Send(bg, d.ID))
	e.clock.Advance(UndoSendWindow)
	e.sync(t)
	got := e.smtp.Messages()
	require.Len(t, got, 1)
	assert.Equal(t, "alice@example.com", got[0].From, "the envelope sender is the account's")
	m, err := netmail.ReadMessage(bytes.NewReader(got[0].Data))
	require.NoError(t, err)
	assertFrom(t, m.Header.Get("From"), "Belcova", "contact@belcova.fr")
	rt, err := m.Header.AddressList("Reply-To")
	require.NoError(t, err)
	assert.Equal(t, "team@belcova.fr", rt[0].Address)
}

// A reply is sent as the identity the mail was sent to, as MailMate and
// Thunderbird do, and leaves every address of the user out of its recipients.
func TestReplyIdentity(t *testing.T) {
	e := newSendEnv(t, false)
	contact, err := e.s.AddIdentity(bg, e.acc.ID, IdentityInput{Name: "Belcova", Email: "contact@belcova.fr"})
	require.NoError(t, err)
	sales, err := e.s.AddIdentity(bg, e.acc.ID, IdentityInput{Name: "Sales", Email: "sales@belcova.fr", IsDefault: true})
	require.NoError(t, err)
	e.put(t, "INBOX", eml("to-contact@x", "Bob <bob@example.com>", "Belcova <contact@belcova.fr>, sales@belcova.fr", "quote", day, "price?", "Cc: dan@example.com, alice@example.com"))
	e.put(t, "INBOX", eml("cc-contact@x", "Bob <bob@example.com>", "carol@example.com", "fyi", day, "fyi", "Cc: CONTACT@belcova.fr"))
	e.put(t, "INBOX", eml("to-alice@x", "Bob <bob@example.com>", "alice@example.com", "hi", day, "hi"))
	e.put(t, "INBOX", eml("to-list@x", "Bob <bob@example.com>", "list@lists.example", "news", day, "news"))
	e.put(t, "INBOX", eml("mine@x", "Belcova <contact@belcova.fr>", "carol@example.com", "offer", day, "offer"))
	e.sync(t)

	for _, c := range []struct {
		msg, mode string
		want      int64
		to, cc    []string
	}{
		{"to-contact@x", "reply_all", contact.ID, []string{"Bob <bob@example.com>"}, []string{"dan@example.com"}},
		{"cc-contact@x", "reply", contact.ID, []string{"Bob <bob@example.com>"}, nil},
		{"to-alice@x", "reply", 0, []string{"Bob <bob@example.com>"}, nil},
		{"to-list@x", "reply", sales.ID, []string{"Bob <bob@example.com>"}, nil},
		{"mine@x", "reply", contact.ID, []string{"carol@example.com"}, nil},
		{"to-contact@x", "forward", contact.ID, nil, nil},
	} {
		d, err := e.s.NewDraft(bg, e.acc.ID, c.mode, e.idOf(t, c.msg))
		require.NoError(t, err, c.msg)
		assert.Equal(t, c.want, d.IdentityID, c.msg)
		assert.Equal(t, c.to, d.To, c.msg)
		assert.Equal(t, c.cc, d.Cc, c.msg)
	}
}

// Mail from an identity's address is the user's own: a conversation is not
// listed as from it, and a reply to it follows up with its recipients.
func TestIdentityMailIsOwn(t *testing.T) {
	e := newSendEnv(t, false)
	_, err := e.s.AddIdentity(bg, e.acc.ID, IdentityInput{Name: "Belcova", Email: "contact@belcova.fr"})
	require.NoError(t, err)
	e.put(t, "INBOX", eml("q@x", "Bob <bob@example.com>", "contact@belcova.fr", "quote", day, "price?"))
	e.put(t, "INBOX", eml("a@x", "Belcova <contact@belcova.fr>", "bob@example.com", "Re: quote", "Mon, 05 Oct 2026 10:00:00 +0000", "42", "In-Reply-To: <q@x>", "References: <q@x>"))
	e.sync(t)
	page, err := e.s.ListThreads(bg, View{Unified: "inbox"}, "", 10)
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	assert.Equal(t, "bob@example.com", mail.AddrKey(page.Items[0].From), "the user's own reply is not the conversation's sender")
}
