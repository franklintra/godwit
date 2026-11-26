package mail_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/godwit-email/godwit/core/mail"
	"github.com/godwit-email/godwit/internal/gpg"
	"github.com/godwit-email/godwit/internal/gpg/gpgtest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pgpOutgoing has what tends to break a signature: non-ASCII text and
// names, a line starting "From ", trailing spaces, an inline image and an
// attachment.
func pgpOutgoing() mail.Outgoing {
	return mail.Outgoing{
		From: "Frànklin <me@x.com>", To: []string{"Zoë <a@y.com>"}, Bcc: []string{"hidden@z.com"},
		Subject:     "Café ☕ signé",
		HTML:        `<p>Bonjour à tous   </p><p>From the start, ça marche.</p><p>From here too</p><img src="cid:logo@x">`,
		Related:     []mail.Attachment{{ContentID: "logo@x", ContentType: "image/png", Data: []byte("\x89PNG fake")}},
		Attachments: []mail.Attachment{{Filename: "résumé.pdf", ContentType: "application/pdf", Data: bytes.Repeat([]byte{0, 1, 2, 255}, 300)}},
	}
}

func TestBuildSigned(t *testing.T) {
	var signed []byte
	o := pgpOutgoing()
	o.OpenPGP = &mail.OpenPGP{Sign: func(data []byte) ([]byte, string, error) {
		signed = append([]byte(nil), data...)
		return []byte("-----BEGIN PGP SIGNATURE-----\n\nfake\n-----END PGP SIGNATURE-----\n"), "pgp-sha256", nil
	}}
	raw, id, err := mail.Build(o, time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	s := string(raw)
	assert.NotContains(t, s, "hidden@z.com")
	assert.Contains(t, s, "micalg=pgp-sha256")
	assert.Contains(t, s, `protocol="application/pgp-signature"`)
	assert.NotContains(t, strings.ReplaceAll(s, "\r\n", ""), "\n", "every line ends in CRLF")
	for i, c := range signed {
		require.Less(t, c, byte(128), "byte %d of the signed entity is not 7-bit", i)
	}
	assert.NotContains(t, string(signed), "\r\nFrom ", "a line starting From is escaped")
	assert.True(t, bytes.HasPrefix(signed, []byte("Content-Type: multipart/")), "the entity starts with its Content-Type: %q", signed[:40])

	// What a verifier extracts is exactly what was signed.
	p, err := mail.FindPGPMIME(raw)
	require.NoError(t, err)
	require.NotNil(t, p)
	assert.Equal(t, "signed", p.Kind)
	assert.True(t, p.Whole)
	assert.Equal(t, signed, p.Signed)
	assert.Equal(t, "-----BEGIN PGP SIGNATURE-----\r\n\r\nfake\r\n-----END PGP SIGNATURE-----", string(p.Signature))

	// A mail program without OpenPGP still reads it, and the headers stay outside.
	parsed, err := mail.Parse(raw)
	require.NoError(t, err)
	assert.False(t, parsed.Damaged, parsed.Warnings)
	assert.Equal(t, id, parsed.MessageID)
	assert.Equal(t, "Café ☕ signé", parsed.Subject)
	assert.Equal(t, []string{"Zoë <a@y.com>"}, parsed.To)
	assert.Contains(t, parsed.Text, "From the start, ça marche.")
	assert.Contains(t, parsed.Text, "\nFrom here too")
	assert.Contains(t, parsed.HTML, "Bonjour à tous")
	var names []string
	for _, a := range parsed.Attachments {
		names = append(names, a.Filename)
	}
	assert.ElementsMatch(t, []string{"", "résumé.pdf", "signature.asc"}, names) // the inline image has no name
}

func TestBuildEncrypted(t *testing.T) {
	var plain []byte
	o := pgpOutgoing()
	o.OpenPGP = &mail.OpenPGP{
		Sign: func([]byte) ([]byte, string, error) {
			t.Fatal("encrypting signs inside the OpenPGP message")
			return nil, "", nil
		},
		Encrypt: func(data []byte) ([]byte, error) {
			plain = append([]byte(nil), data...)
			return []byte("-----BEGIN PGP MESSAGE-----\n\n" + base64.StdEncoding.EncodeToString(data[:20]) + "\n-----END PGP MESSAGE-----\n"), nil
		},
	}
	raw, _, err := mail.Build(o, time.Now())
	require.NoError(t, err)
	s := string(raw)
	assert.Contains(t, s, `Content-Type: multipart/encrypted;`)
	assert.Contains(t, s, "Version: 1")
	assert.NotContains(t, s, "Bonjour", "nothing of the body shows outside")
	assert.NotContains(t, s, "résumé")
	p, err := mail.FindPGPMIME(raw)
	require.NoError(t, err)
	require.NotNil(t, p)
	assert.Equal(t, "encrypted", p.Kind)
	assert.True(t, bytes.HasPrefix(p.Encrypted, []byte("-----BEGIN PGP MESSAGE-----")))
	inner, err := mail.Parse(plain)
	require.NoError(t, err)
	assert.Contains(t, inner.Text, "From the start")
}

// The real thing: gpg signs and encrypts what Build hands it, and gpg on
// the other side verifies and decrypts what FindPGPMIME extracts.
func TestBuildWithGPG(t *testing.T) {
	alice, bob := gpgtest.New(t), gpgtest.New(t)
	af, bf := alice.Gen("Alice <me@x.com>"), bob.Gen("Bob <a@y.com>")
	alice.Import(bob.Export(bf))
	bob.Import(alice.Export(af))
	alice.Certify(bf)
	ctx := context.Background()

	o := pgpOutgoing()
	o.OpenPGP = &mail.OpenPGP{Sign: func(data []byte) ([]byte, string, error) {
		sig, h, err := alice.GPG.Sign(ctx, af, data)
		return sig, gpg.Micalg(h), err
	}}
	raw, _, err := mail.Build(o, time.Now())
	require.NoError(t, err)
	// Delivered to a Unix mailbox: LF line ends. Verifying makes them CRLF again.
	lf := bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n"))
	p, err := mail.FindPGPMIME(lf)
	require.NoError(t, err)
	require.NotNil(t, p)
	r, err := bob.GPG.Verify(ctx, p.Signature, p.Signed)
	require.NoError(t, err)
	require.Len(t, r.Signatures, 1)
	assert.Equal(t, gpg.SigGood, r.Signatures[0].Status)
	assert.Equal(t, af, r.Signatures[0].PrimaryFingerprint)

	// A changed character in the body breaks it.
	tampered := bytes.Replace(raw, []byte("marche"), []byte("marchE"), 1)
	p, err = mail.FindPGPMIME(tampered)
	require.NoError(t, err)
	r, err = bob.GPG.Verify(ctx, p.Signature, p.Signed)
	require.NoError(t, err)
	assert.Equal(t, gpg.SigBad, r.Signatures[0].Status)

	o.OpenPGP = &mail.OpenPGP{Encrypt: func(data []byte) ([]byte, error) {
		return alice.GPG.Encrypt(ctx, gpg.EncryptOptions{Recipients: []string{bf, af}, SignKey: af}, data)
	}}
	raw, _, err = mail.Build(o, time.Now())
	require.NoError(t, err)
	p, err = mail.FindPGPMIME(raw)
	require.NoError(t, err)
	require.NotNil(t, p)
	plain, res, err := bob.GPG.Decrypt(ctx, p.Encrypted)
	require.NoError(t, err)
	assert.True(t, res.Decrypted)
	require.Len(t, res.Signatures, 1)
	assert.Equal(t, gpg.SigGood, res.Signatures[0].Status)
	inner, err := mail.Parse(plain)
	require.NoError(t, err)
	assert.Contains(t, inner.HTML, "Bonjour à tous")
	require.Len(t, inner.Attachments, 2)
}

func TestFindPGPMIMENested(t *testing.T) {
	// A mailing list wrapped the signed message and added a footer.
	raw := "From: a@x\r\nContent-Type: multipart/mixed; boundary=outer\r\n\r\n" +
		"--outer\r\n" +
		"Content-Type: multipart/signed; boundary=\"in\"; micalg=pgp-sha256; protocol=\"application/pgp-signature\"\r\n\r\n" +
		"preamble\r\n--in\r\nContent-Type: text/plain\r\n\r\nsigned text\r\n--in\r\n" +
		"Content-Type: application/pgp-signature\r\n\r\nSIG\r\n--in--\r\n" +
		"\r\n--outer\r\nContent-Type: text/plain\r\n\r\nlist footer\r\n--outer--\r\n"
	p, err := mail.FindPGPMIME([]byte(raw))
	require.NoError(t, err)
	require.NotNil(t, p)
	assert.Equal(t, "Content-Type: text/plain\r\n\r\nsigned text", string(p.Signed))
	assert.False(t, p.Whole, "a footer was added outside the signature")
	assert.Equal(t, "SIG", string(p.Signature))

	none, err := mail.FindPGPMIME([]byte("From: a@x\r\nContent-Type: text/plain\r\n\r\nhi\r\n"))
	assert.NoError(t, err)
	assert.Nil(t, none)
}

func TestFindPGPMIMEBoundaryPrefix(t *testing.T) {
	// "--in" must not end a part at "--inner", nor at a line that merely contains it.
	raw := "Content-Type: multipart/signed; boundary=in; protocol=\"application/pgp-signature\"\r\n\r\n" +
		"--in  \r\nContent-Type: text/plain\r\n\r\n--inner is text\r\nnot --in\r\n--in\r\n" +
		"Content-Type: application/pgp-signature\r\n\r\nSIG\r\n--in--"
	p, err := mail.FindPGPMIME([]byte(raw))
	require.NoError(t, err)
	require.NotNil(t, p)
	assert.Equal(t, "Content-Type: text/plain\r\n\r\n--inner is text\r\nnot --in", string(p.Signed))
	assert.Equal(t, "SIG", string(p.Signature))
}

func TestFindInlinePGP(t *testing.T) {
	block := "-----BEGIN PGP SIGNED MESSAGE-----\r\nHash: SHA512\r\n\r\nCaf=E9\r\n-----BEGIN PGP SIGNATURE-----\r\n\r\nabc\r\n-----END PGP SIGNATURE-----\r\n"
	raw := "Content-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\n" +
		"Content-Type: text/plain; charset=iso-8859-1\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n" +
		"Hello\r\n" + block + "bye\r\n--b--\r\n"
	p, err := mail.FindInlinePGP([]byte(raw))
	require.NoError(t, err)
	require.NotNil(t, p)
	assert.Equal(t, "signed", p.Kind)
	assert.Equal(t, "iso-8859-1", p.Charset)
	assert.False(t, p.Whole)
	// Decoded from quoted-printable, but still in its own charset: what was signed.
	assert.Equal(t, strings.Replace(block, "Caf=E9", "Caf\xe9", 1), string(p.Block))

	enc := "Content-Type: text/plain\r\n\r\n-----BEGIN PGP MESSAGE-----\r\n\r\nxyz\r\n-----END PGP MESSAGE-----\r\n"
	p, err = mail.FindInlinePGP([]byte(enc))
	require.NoError(t, err)
	require.NotNil(t, p)
	assert.Equal(t, "message", p.Kind)
	assert.True(t, p.Whole)

	// Quoted in a reply, not at a line start: not a block.
	p, err = mail.FindInlinePGP([]byte("Content-Type: text/plain\r\n\r\n> -----BEGIN PGP MESSAGE-----\r\n> x\r\n> -----END PGP MESSAGE-----\r\n"))
	assert.NoError(t, err)
	assert.Nil(t, p)
}
