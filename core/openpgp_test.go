package core

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/godwit-email/godwit/core/mail"
	"github.com/godwit-email/godwit/core/store"
	"github.com/godwit-email/godwit/internal/gpg"
	"github.com/godwit-email/godwit/internal/gpg/gpgtest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pgpEnv is a send environment whose user (alice@example.com) has a key in
// the keyring Godwit's gpg uses, and Bob, a correspondent with his own.
type pgpEnv struct {
	*sendEnv
	alice, bob       *gpgtest.Keyring
	aliceFpr, bobFpr string
}

func newPGPEnv(t *testing.T) *pgpEnv {
	t.Helper()
	alice, bob := gpgtest.New(t), gpgtest.New(t)
	e := &pgpEnv{sendEnv: newSendEnv(t, false), alice: alice, bob: bob}
	e.aliceFpr, e.bobFpr = alice.Gen("Alice <alice@example.com>"), bob.Gen("Bob <bob@example.com>")
	alice.Import(bob.Export(e.bobFpr))
	bob.Import(alice.Export(e.aliceFpr))
	t.Setenv("GNUPGHOME", alice.GPG.Home) // the user's keyring, as gpg finds it
	return e
}

func (e *pgpEnv) draftTo(t *testing.T, to ...string) Draft {
	t.Helper()
	d, err := e.s.NewDraft(bg, e.acc.ID, "new", 0)
	require.NoError(t, err)
	d.To, d.Subject, d.HTML = to, "Café", "<p>Bonjour, c'est signé.</p><p>From now on.</p>"
	d, err = e.s.SaveDraft(bg, d)
	require.NoError(t, err)
	return d
}

func (e *pgpEnv) sent(t *testing.T) []byte {
	t.Helper()
	e.clock.Advance(UndoSendWindow)
	e.sync(t)
	got := e.smtp.Messages()
	require.NotEmpty(t, got)
	return got[len(got)-1].Data
}

func TestOpenPGPAccountSettings(t *testing.T) {
	e := newPGPEnv(t)
	st, err := e.s.OpenPGPStatus(bg)
	require.NoError(t, err)
	assert.Empty(t, st.Error)
	assert.True(t, strings.HasPrefix(st.Version, "2."), st.Version)

	keys, err := e.s.OpenPGPKeys(bg)
	require.NoError(t, err)
	require.Len(t, keys, 1)
	assert.Equal(t, e.aliceFpr, keys[0].Fingerprint)
	assert.True(t, keys[0].CanSign)
	assert.Equal(t, []string{"Alice <alice@example.com>"}, keys[0].UserIDs)

	_, err = e.s.SetAccountOpenPGP(bg, e.acc.ID, AccountOpenPGP{Key: e.bobFpr})
	assert.ErrorIs(t, err, ErrOpenPGPNoKey, "only a secret key the user has")

	d := e.draftTo(t, "bob@example.com")
	assert.Equal(t, DraftOpenPGP{Sign: false, Encrypt: OpenPGPEncryptOff}, d.OpenPGP, "no key: OpenPGP off")
	_, err = e.s.SetDraftOpenPGP(bg, d.ID, d.Revision, DraftOpenPGP{Sign: true, Encrypt: OpenPGPEncryptOff})
	assert.ErrorIs(t, err, ErrOpenPGPNoKey)

	a, err := e.s.SetAccountOpenPGP(bg, e.acc.ID, AccountOpenPGP{Key: strings.ToLower(e.aliceFpr), SignByDefault: true, EncryptWhenPossible: true})
	require.NoError(t, err)
	assert.Equal(t, AccountOpenPGP{Key: e.aliceFpr, SignByDefault: true, EncryptWhenPossible: true}, a)
	got, err := e.s.GetDraft(bg, d.ID)
	require.NoError(t, err)
	assert.Equal(t, DraftOpenPGP{Sign: true, Encrypt: OpenPGPEncryptAuto}, got.OpenPGP, "a draft follows its account until set")

	got, err = e.s.SetDraftOpenPGP(bg, d.ID, got.Revision, DraftOpenPGP{Sign: false, Encrypt: OpenPGPEncryptOn})
	require.NoError(t, err)
	assert.Equal(t, DraftOpenPGP{Sign: false, Encrypt: OpenPGPEncryptOn}, got.OpenPGP)
	assert.Equal(t, d.Revision+1, got.Revision)
	_, err = e.s.SetDraftOpenPGP(bg, d.ID, d.Revision, DraftOpenPGP{Sign: true, Encrypt: OpenPGPEncryptOn})
	assert.ErrorIs(t, err, ErrStaleDraft)
	_, err = e.s.SetAccountOpenPGP(bg, e.acc.ID, AccountOpenPGP{Key: e.aliceFpr})
	require.NoError(t, err)
	got, err = e.s.GetDraft(bg, d.ID)
	require.NoError(t, err)
	assert.Equal(t, DraftOpenPGP{Sign: false, Encrypt: OpenPGPEncryptOn}, got.OpenPGP, "set on the draft, it stays")
}

// What Franklin does every day: a signed message, verified by its recipient,
// and the same signed message kept in Sent.
func TestOpenPGPSignedSend(t *testing.T) {
	e := newPGPEnv(t)
	_, err := e.s.SetAccountOpenPGP(bg, e.acc.ID, AccountOpenPGP{Key: e.aliceFpr, SignByDefault: true})
	require.NoError(t, err)
	d := e.draftTo(t, "Bob <bob@example.com>")
	require.True(t, d.OpenPGP.Sign)
	require.NoError(t, e.s.Send(bg, d.ID))
	raw := e.sent(t)
	assert.Contains(t, string(raw), "Content-Type: multipart/signed;")

	p, err := mail.FindPGPMIME(raw)
	require.NoError(t, err)
	require.NotNil(t, p)
	r, err := e.bob.GPG.Verify(context.Background(), p.Signature, p.Signed)
	require.NoError(t, err)
	require.Len(t, r.Signatures, 1)
	assert.Equal(t, gpg.SigGood, r.Signatures[0].Status)
	assert.Equal(t, e.aliceFpr, r.Signatures[0].PrimaryFingerprint)

	// The Sent copy is the signed message itself, and it verifies in the reader.
	e.sync(t)
	mid := store.Str(e.rows(t, "SELECT message_id_hdr FROM outbox ORDER BY id DESC LIMIT 1")[0][0])
	sentID := e.idOf(t, mid)
	v, err := e.s.OpenPGPMessage(bg, sentID)
	require.NoError(t, err)
	assert.Equal(t, "signed", v.Kind)
	require.NotNil(t, v.Signature)
	assert.Equal(t, OpenPGPVerified, v.Signature.Status, "%+v", v.Signature)
	assert.True(t, v.Signature.MatchesSender)
	assert.False(t, v.Partial)
}

func TestOpenPGPEncryptedSend(t *testing.T) {
	e := newPGPEnv(t)
	_, err := e.s.SetAccountOpenPGP(bg, e.acc.ID, AccountOpenPGP{Key: e.aliceFpr, SignByDefault: true, EncryptWhenPossible: true})
	require.NoError(t, err)

	// Bob's key is not valid for Alice until she certifies it: auto sends signed only.
	keys, err := e.s.OpenPGPRecipientKeys(bg, []string{"Bob <bob@example.com>", "carol@example.com"})
	require.NoError(t, err)
	assert.Equal(t, []OpenPGPRecipient{{Address: "bob@example.com"}, {Address: "carol@example.com"}}, keys)
	d := e.draftTo(t, "bob@example.com")
	require.NoError(t, e.s.Send(bg, d.ID))
	assert.Contains(t, string(e.sent(t)), "multipart/signed")

	e.alice.Certify(e.bobFpr)
	keys, err = e.s.OpenPGPRecipientKeys(bg, []string{"bob@example.com"})
	require.NoError(t, err)
	assert.Equal(t, e.bobFpr, keys[0].Key)
	d = e.draftTo(t, "bob@example.com")
	d.Bcc = []string{"alice@example.com"}
	d, err = e.s.SaveDraft(bg, d)
	require.NoError(t, err)
	require.NoError(t, e.s.Send(bg, d.ID))
	raw := e.sent(t)
	assert.Contains(t, string(raw), "multipart/encrypted")
	assert.NotContains(t, string(raw), "Bonjour")
	p, err := mail.FindPGPMIME(raw)
	require.NoError(t, err)
	plain, r, err := e.bob.GPG.Decrypt(context.Background(), p.Encrypted)
	require.NoError(t, err)
	assert.True(t, r.Decrypted)
	require.Len(t, r.Signatures, 1, "signed inside")
	assert.Equal(t, gpg.SigGood, r.Signatures[0].Status)
	inner, err := mail.Parse(plain)
	require.NoError(t, err)
	assert.Contains(t, inner.Text, "Bonjour, c'est signé.")

	// Encryption required and Carol has no key: nothing is sent, the draft stays.
	d = e.draftTo(t, "bob@example.com", "carol@example.com")
	d, err = e.s.SetDraftOpenPGP(bg, d.ID, d.Revision, DraftOpenPGP{Sign: true, Encrypt: OpenPGPEncryptOn})
	require.NoError(t, err)
	err = e.s.Send(bg, d.ID)
	var missing *OpenPGPKeysMissingError
	require.ErrorAs(t, err, &missing)
	assert.Equal(t, []string{"carol@example.com"}, missing.Addresses)
	_, err = e.s.GetDraft(bg, d.ID)
	assert.NoError(t, err, "the draft is kept")
	assert.Zero(t, e.count(t, "SELECT count(*) FROM outbox WHERE draft_id = ?", d.ID))
}

func TestSetDraftOpenPGPProgramCannotDowngrade(t *testing.T) {
	e := newPGPEnv(t)
	_, err := e.s.SetAccountOpenPGP(bg, e.acc.ID, AccountOpenPGP{Key: e.aliceFpr, SignByDefault: true})
	require.NoError(t, err)
	d := e.draftTo(t, "bob@example.com")
	mcp := WithExternalSource(bg, AuditMCP)
	_, err = e.s.SetDraftOpenPGP(mcp, d.ID, d.Revision, DraftOpenPGP{Sign: false, Encrypt: OpenPGPEncryptOff})
	assert.ErrorIs(t, err, ErrOpenPGPDowngrade)
	d, err = e.s.SetDraftOpenPGP(mcp, d.ID, d.Revision, DraftOpenPGP{Sign: true, Encrypt: OpenPGPEncryptOn})
	require.NoError(t, err, "a program may ask for more")
	assert.True(t, d.FromProgram)
	_, err = e.s.SetDraftOpenPGP(withProgram(bg), d.ID, d.Revision, DraftOpenPGP{Sign: true, Encrypt: OpenPGPEncryptAuto})
	assert.ErrorIs(t, err, ErrOpenPGPDowngrade, "a plugin or Wit neither")
	d, err = e.s.SetDraftOpenPGP(WithExternalSource(bg, AuditLocalAPI), d.ID, d.Revision, DraftOpenPGP{Sign: false, Encrypt: OpenPGPEncryptOff})
	require.NoError(t, err, "the user's own program over the full role may")
	_, err = e.s.SetDraftOpenPGP(bg, d.ID, d.Revision, DraftOpenPGP{Sign: true, Encrypt: "maybe"})
	assert.Error(t, err)
}

// bobSends builds a message from Bob to Alice, sealed by seal, and puts it
// in Alice's inbox.
func (e *pgpEnv) bobSends(t *testing.T, seal func(*mail.Outgoing)) int64 {
	t.Helper()
	o := mail.Outgoing{From: "Bob <bob@example.com>", To: []string{"alice@example.com"}, Subject: "Re: Café",
		HTML: "<p>The secret word is <b>pelican</b>.</p>"}
	seal(&o)
	raw, id, err := mail.Build(o, time.Now())
	require.NoError(t, err)
	e.put(t, "INBOX", string(raw))
	e.sync(t)
	return e.idOf(t, id)
}

func TestOpenPGPMessageVerify(t *testing.T) {
	e := newPGPEnv(t)
	ctx := context.Background()
	sign := func(o *mail.Outgoing) {
		o.OpenPGP = &mail.OpenPGP{Sign: func(data []byte) ([]byte, string, error) {
			sig, h, err := e.bob.GPG.Sign(ctx, e.bobFpr, data)
			return sig, gpg.Micalg(h), err
		}}
	}
	id := e.bobSends(t, sign)
	m, err := e.s.GetMessage(bg, id, false)
	require.NoError(t, err)
	assert.Equal(t, "signed", OpenPGPKind(m))
	v, err := e.s.OpenPGPMessage(bg, id)
	require.NoError(t, err)
	require.NotNil(t, v.Signature)
	assert.Equal(t, OpenPGPUnverified, v.Signature.Status, "Bob's key is not certified: good, not verified")
	assert.Equal(t, "good", v.Signature.Detail)
	assert.Equal(t, e.bobFpr, v.Signature.Fingerprint)
	assert.True(t, v.Signature.MatchesSender)

	e.alice.Certify(e.bobFpr)
	v, err = e.s.OpenPGPMessage(bg, id)
	require.NoError(t, err)
	assert.Equal(t, OpenPGPUnverified, v.Signature.Status, "the result is reused for a while")
	e.s.openpgp = openpgpCache{}
	v, err = e.s.OpenPGPMessage(bg, id)
	require.NoError(t, err)
	assert.Equal(t, OpenPGPVerified, v.Signature.Status)

	// Signed by Bob's key, but sent as someone else.
	mallory := e.bobSends(t, func(o *mail.Outgoing) {
		sign(o)
		o.From = "Mallory <mallory@example.com>"
	})
	v, err = e.s.OpenPGPMessage(bg, mallory)
	require.NoError(t, err)
	assert.Equal(t, OpenPGPUnverified, v.Signature.Status)
	assert.False(t, v.Signature.MatchesSender)

	// A stranger's signature.
	eve := gpgtest.New(t)
	eveFpr := eve.Gen("Eve <eve@example.com>")
	strange := e.bobSends(t, func(o *mail.Outgoing) {
		o.OpenPGP = &mail.OpenPGP{Sign: func(data []byte) ([]byte, string, error) {
			sig, h, err := eve.GPG.Sign(ctx, eveFpr, data)
			return sig, gpg.Micalg(h), err
		}}
	})
	v, err = e.s.OpenPGPMessage(bg, strange)
	require.NoError(t, err)
	assert.Equal(t, OpenPGPUnknownKey, v.Signature.Status)

	plain := e.bobSends(t, func(*mail.Outgoing) {})
	v, err = e.s.OpenPGPMessage(bg, plain)
	require.NoError(t, err)
	assert.Equal(t, OpenPGPView{}, v, "no OpenPGP, nothing run")
}

func TestOpenPGPMessageDecrypt(t *testing.T) {
	e := newPGPEnv(t)
	ctx := context.Background()
	e.bob.Certify(e.aliceFpr)
	id := e.bobSends(t, func(o *mail.Outgoing) {
		o.OpenPGP = &mail.OpenPGP{Encrypt: func(data []byte) ([]byte, error) {
			return e.bob.GPG.Encrypt(ctx, gpg.EncryptOptions{Recipients: []string{e.aliceFpr, e.bobFpr}, SignKey: e.bobFpr}, data)
		}}
	})
	m, err := e.s.GetMessage(bg, id, false)
	require.NoError(t, err)
	assert.Equal(t, "encrypted", OpenPGPKind(m))
	assert.NotContains(t, m.Text+m.HTML, "pelican")

	v, err := e.s.OpenPGPMessage(bg, id)
	require.NoError(t, err)
	assert.Equal(t, "encrypted", v.Kind)
	assert.True(t, v.Decrypted, v.DecryptError)
	assert.Contains(t, v.HTML, "<b>pelican</b>")
	assert.Contains(t, v.Text, "pelican")
	require.NotNil(t, v.Signature)
	assert.Equal(t, "good", v.Signature.Detail)

	// Nothing decrypted is stored or searchable.
	assert.Zero(t, e.count(t, "SELECT count(*) FROM messages WHERE body_text LIKE '%pelican%' OR body_html LIKE '%pelican%' OR snippet LIKE '%pelican%'"))
	found, err := e.s.Search(bg, e.acc.ID, "pelican", 10)
	require.NoError(t, err)
	assert.Empty(t, found)

	// Encrypted to someone else only.
	eve := gpgtest.New(t)
	eveFpr := eve.Gen("Eve <eve@example.com>")
	bobOnly := e.bobSends(t, func(o *mail.Outgoing) {
		o.OpenPGP = &mail.OpenPGP{Encrypt: func(data []byte) ([]byte, error) {
			eve.Import(e.bob.Export(e.bobFpr))
			eve.Certify(e.bobFpr)
			return eve.GPG.Encrypt(ctx, gpg.EncryptOptions{Recipients: []string{e.bobFpr, eveFpr}}, data)
		}}
	})
	v, err = e.s.OpenPGPMessage(bg, bobOnly)
	require.NoError(t, err)
	assert.False(t, v.Decrypted)
	assert.Equal(t, "no_secret_key", v.DecryptError)
	assert.Empty(t, v.HTML)
}

func TestOpenPGPNoGPG(t *testing.T) {
	e := newSendEnv(t, false)
	t.Setenv("PATH", t.TempDir())
	_, err := e.s.SetGPGPath(bg, "/nonexistent/gpg")
	assert.ErrorIs(t, err, ErrGPGNotFound)
	if _, err := e.s.OpenPGPKeys(bg); err != nil && !errors.Is(err, ErrGPGNotFound) {
		t.Errorf("without gpg on PATH: %v", err) // a gpg in a usual place may still be found
	}
}
