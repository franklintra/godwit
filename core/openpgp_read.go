package core

import (
	"context"
	"errors"
	netmail "net/mail"
	"strings"
	"time"

	"github.com/godwit-email/godwit/core/mail"
	"github.com/godwit-email/godwit/internal/gpg"
)

// What OpenPGP says of a received message's signature (OpenPGPSignature.Status).
const (
	OpenPGPVerified   = "verified"    // good, by a key the trust model vouches for, whose user ID has the sender's address
	OpenPGPUnverified = "unverified"  // good, but the key is not vouched for, does not match the sender, or has expired or been revoked
	OpenPGPUnknownKey = "unknown_key" // the signer's key is not in the keyring
	OpenPGPBad        = "bad"         // the message or the signature was changed
	SigCheckFailed    = "error"       // gpg could not check it
)

// OpenPGPSignature is a received message's signature as gpg checked it.
type OpenPGPSignature struct {
	Status string // SigVerified … SigCheckFailed
	// Detail is gpg's finding: good, expired, expired_key, revoked_key, bad,
	// no_public_key or error.
	Detail      string
	Fingerprint string // the signer's primary key; "" without the key
	KeyID       string
	UserID      string // the signer's user ID as gpg names it
	Created     time.Time
	// Trust is the validity the user's trust model (TOFU, the web of trust)
	// gives the key: undefined, never, marginal, fully or ultimate; "" for
	// none. TrustModel names the model.
	Trust, TrustModel string
	// MatchesSender: one of the key's user IDs has the From address.
	MatchesSender bool
}

// OpenPGPAttachment is an attachment inside an encrypted message.
type OpenPGPAttachment struct {
	Filename, ContentType string
	Size                  int
}

// OpenPGPView is what OpenPGP says of a received message (OpenPGPMessage).
type OpenPGPView struct {
	// Kind is "" (no OpenPGP), "signed" or "encrypted"; Inline when it is
	// armor in the text rather than PGP/MIME.
	Kind   string
	Inline bool
	// Partial: the signed or encrypted part is not the whole message, so
	// what shows around it is neither signed nor encrypted.
	Partial   bool
	Signature *OpenPGPSignature // nil when there is none
	// Decrypted: it was encrypted and gpg decrypted it. DecryptError says
	// why it could not: "no_secret_key", or gpg's own words.
	Decrypted    bool
	DecryptError string
	// The decrypted message: its protected Subject if it has one, its text,
	// its HTML sanitized with remote images blocked, and its attachments.
	// Nothing decrypted is ever stored or indexed for search.
	Subject, Text, HTML string
	Attachments         []OpenPGPAttachment
}

// OpenPGPKind is "signed" or "encrypted" when message m (as GetMessage
// returns it) looks signed or encrypted with OpenPGP, else "": what tells
// the reader to ask OpenPGPMessage.
func OpenPGPKind(m Message) string {
	kind := ""
	for _, a := range m.Attachments {
		switch strings.ToLower(a.ContentType) {
		case "application/pgp-encrypted":
			return "encrypted"
		case "application/pgp-signature":
			kind = "signed"
		}
	}
	switch {
	case strings.Contains(m.Text, "-----BEGIN PGP MESSAGE-----"):
		return "encrypted"
	case kind == "" && strings.Contains(m.Text, "-----BEGIN PGP SIGNED MESSAGE-----"):
		return "signed"
	}
	return kind
}

// OpenPGPMessage checks a received message's OpenPGP signature and decrypts
// it, through the user's gpg: it downloads the original from the server (or
// reads a local archive's file). gpg-agent may ask for a passphrase to
// decrypt. What is decrypted is returned, never stored. Only Godwit's
// interface calls it (rpcbind uiOnly): it hands out decrypted mail.
func (s *Service) OpenPGPMessage(ctx context.Context, messageID int64) (OpenPGPView, error) {
	m, err := s.readMessage(ctx, messageID, false, true)
	if err != nil {
		return OpenPGPView{}, err
	}
	if OpenPGPKind(m) == "" {
		return OpenPGPView{}, nil
	}
	g, err := s.gpgRunner(ctx)
	if err != nil {
		return OpenPGPView{}, err
	}
	raw, err := s.GetRawMessage(ctx, messageID)
	if err != nil {
		return OpenPGPView{}, err
	}
	from := ""
	if a, err := netmail.ParseAddress(m.From); err == nil {
		from = a.Address
	}
	p, err := mail.FindPGPMIME(raw)
	if err != nil {
		return OpenPGPView{}, err
	}
	if p != nil {
		v := OpenPGPView{Kind: p.Kind, Partial: !p.Whole}
		if p.Kind == "signed" {
			r, err := g.Verify(ctx, p.Signature, p.Signed)
			v.Signature = s.signatureView(ctx, g, r, err, from)
			return v, nil
		}
		s.decryptInto(ctx, g, &v, p.Encrypted, from, "")
		return v, nil
	}
	in, err := mail.FindInlinePGP(raw)
	if err != nil || in == nil {
		return OpenPGPView{}, err
	}
	v := OpenPGPView{Inline: true, Partial: !in.Whole}
	if in.Kind == "signed" {
		v.Kind = "signed"
		r, err := g.VerifyClearsigned(ctx, in.Block)
		v.Signature = s.signatureView(ctx, g, r, err, from)
		return v, nil
	}
	v.Kind = "encrypted"
	s.decryptInto(ctx, g, &v, in.Block, from, in.Charset)
	return v, nil
}

// decryptInto decrypts data into v. charset is an inline message's text's
// own; "" for PGP/MIME, whose plaintext is a MIME entity.
func (s *Service) decryptInto(ctx context.Context, g *gpg.GPG, v *OpenPGPView, data []byte, from, charset string) {
	plain, r, err := g.Decrypt(ctx, data)
	switch {
	case errors.Is(err, gpg.ErrNoSecretKey):
		v.DecryptError = "no_secret_key"
		return
	case err != nil:
		var oe *OpenPGPError
		if errors.As(openpgpError("decryption", err), &oe) {
			v.DecryptError = oe.Message
		} else {
			v.DecryptError = err.Error()
		}
		return
	case !r.Encrypted: // inline armor that was only signed
		v.Kind = "signed"
	default:
		v.Decrypted = true
	}
	if len(r.Signatures) > 0 {
		v.Signature = s.signatureView(ctx, g, r, nil, from)
	}
	if v.Inline {
		text, err := mail.DecodePart(plain, "", charset)
		if err != nil {
			text = plain
		}
		v.Text = string(mail.ValidText(text, "", false))
		return
	}
	// Signed, then encrypted as two layers (RFC 3156 6.1): the signature is inside.
	if inner, err := mail.FindPGPMIME(plain); err == nil && inner != nil && inner.Kind == "signed" && inner.Whole {
		r, err := g.Verify(ctx, inner.Signature, inner.Signed)
		v.Signature = s.signatureView(ctx, g, r, err, from)
		plain = inner.Signed
	}
	parsed, err := mail.Parse(plain)
	if err != nil {
		v.DecryptError = err.Error()
		v.Decrypted = false
		return
	}
	v.Subject, v.Text = parsed.Subject, parsed.Text
	inline := map[string]mail.Attachment{}
	for _, a := range parsed.Attachments {
		if a.ContentID != "" && strings.HasPrefix(a.ContentType, "image/") {
			inline[a.ContentID] = a
			if a.Inline {
				continue
			}
		}
		v.Attachments = append(v.Attachments, OpenPGPAttachment{Filename: a.Filename, ContentType: a.ContentType, Size: a.Size})
	}
	if parsed.HTML != "" {
		// Remote images stay blocked whatever the sender: a decrypted body
		// must not tell anyone it was read (EFAIL's exfiltration channel).
		v.HTML, _, _ = mail.Sanitize(parsed.HTML, inline, false)
	}
}

// signatureView sums up gpg's first signature, checking the key's user IDs
// against the sender's address from.
func (s *Service) signatureView(ctx context.Context, g *gpg.GPG, r gpg.Result, err error, from string) *OpenPGPSignature {
	if err != nil || len(r.Signatures) == 0 {
		return &OpenPGPSignature{Status: SigCheckFailed, Detail: string(gpg.SigError)}
	}
	sig := r.Signatures[0]
	v := &OpenPGPSignature{Detail: string(sig.Status), Fingerprint: sig.PrimaryFingerprint, KeyID: sig.KeyID,
		UserID: sig.UserID, Created: sig.Created, Trust: string(sig.Trust), TrustModel: sig.TrustModel}
	if v.Fingerprint != "" && from != "" {
		if keys, err := g.PublicKeys(ctx, v.Fingerprint); err == nil {
			for _, k := range keys {
				if k.Fingerprint == v.Fingerprint && k.HasEmail(from) {
					v.MatchesSender = true
				}
			}
		}
	}
	switch sig.Status {
	case gpg.SigBad:
		v.Status = OpenPGPBad
	case gpg.SigNoPublicKey:
		v.Status = OpenPGPUnknownKey
	case gpg.SigError:
		v.Status = SigCheckFailed
	case gpg.SigGood:
		v.Status = OpenPGPUnverified
		if sig.Trust.Valid() && v.MatchesSender {
			v.Status = OpenPGPVerified
		}
	default: // expired signature, expired or revoked key
		v.Status = OpenPGPUnverified
	}
	return v
}
