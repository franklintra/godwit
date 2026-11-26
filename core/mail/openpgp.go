package mail

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"strings"
)

// OpenPGP seals an outgoing message with PGP/MIME (RFC 3156). The functions
// do the cryptography (core runs the user's gpg); Build canonicalizes the
// body and wraps the result. Set Encrypt, Sign or both.
type OpenPGP struct {
	// Sign returns an ASCII-armored detached signature of data and its
	// micalg ("pgp-sha256"). Used when Encrypt is nil.
	Sign func(data []byte) (sig []byte, micalg string, err error)
	// Encrypt returns data as an ASCII-armored OpenPGP message: encrypted,
	// and signed inside when the message is to be signed (RFC 3156 6.2).
	Encrypt func(data []byte) ([]byte, error)
}

// Preambles of the PGP/MIME containers, for mail programs without OpenPGP.
const (
	signedPreamble    = "This is an OpenPGP/MIME signed message (RFC 4880 and 3156)\r\n"
	encryptedPreamble = "This is an OpenPGP/MIME encrypted message (RFC 4880 and 3156)\r\n"
)

// sealOpenPGP turns raw, a message Build wrote, into its PGP/MIME form: the
// Content-* header fields and the body become the signed or encrypted
// entity, and the other header fields stay outside.
func sealOpenPGP(raw []byte, p *OpenPGP) ([]byte, error) {
	if p.Sign == nil && p.Encrypt == nil {
		return raw, nil
	}
	fields, body := splitHeader(canonicalCRLF(raw))
	var outer, inner [][]byte
	for _, f := range fields {
		if strings.HasPrefix(strings.ToLower(fieldName(f)), "content-") {
			inner = append(inner, f)
		} else {
			outer = append(outer, f)
		}
	}
	if len(inner) == 0 {
		return nil, errors.New("openpgp: the message has no Content-Type")
	}
	entity := append(bytes.Join(inner, nil), "\r\n"...)
	entity = append(entity, body...)
	entity = escapeFrom(entity)
	if err := sevenBit(entity); err != nil {
		return nil, err
	}

	var out bytes.Buffer
	for _, f := range outer {
		out.Write(f)
	}
	boundary, err := newBoundary(entity)
	if err != nil {
		return nil, err
	}
	if p.Encrypt != nil {
		enc, err := p.Encrypt(entity)
		if err != nil {
			return nil, err
		}
		out.WriteString(`Content-Type: multipart/encrypted; boundary="` + boundary + `";` + "\r\n" +
			` protocol="application/pgp-encrypted"` + "\r\n\r\n")
		out.WriteString(encryptedPreamble)
		out.WriteString("--" + boundary + "\r\n" +
			"Content-Type: application/pgp-encrypted\r\n" +
			"Content-Description: PGP/MIME version identification\r\n\r\n" +
			"Version: 1\r\n\r\n")
		out.WriteString("--" + boundary + "\r\n" +
			`Content-Type: application/octet-stream; name="encrypted.asc"` + "\r\n" +
			"Content-Description: OpenPGP encrypted message\r\n" +
			`Content-Disposition: inline; filename="encrypted.asc"` + "\r\n\r\n")
		out.Write(trimCRLF(canonicalCRLF(enc)))
		out.WriteString("\r\n--" + boundary + "--\r\n")
		return out.Bytes(), nil
	}
	sig, micalg, err := p.Sign(entity)
	if err != nil {
		return nil, err
	}
	if micalg == "" {
		return nil, errors.New("openpgp: the signature's hash algorithm has no micalg name")
	}
	out.WriteString(`Content-Type: multipart/signed; boundary="` + boundary + `"; micalg=` + micalg + ";\r\n" +
		` protocol="application/pgp-signature"` + "\r\n\r\n")
	out.WriteString(signedPreamble)
	// The signed entity is exactly what lies between the delimiter line and
	// the CRLF before the next delimiter (RFC 3156 5, RFC 2046 5.1.1).
	out.WriteString("--" + boundary + "\r\n")
	out.Write(entity)
	out.WriteString("\r\n--" + boundary + "\r\n" +
		`Content-Type: application/pgp-signature; name="signature.asc"` + "\r\n" +
		"Content-Description: OpenPGP digital signature\r\n" +
		`Content-Disposition: attachment; filename="signature.asc"` + "\r\n\r\n")
	out.Write(trimCRLF(canonicalCRLF(sig)))
	out.WriteString("\r\n--" + boundary + "--\r\n")
	return out.Bytes(), nil
}

// canonicalCRLF makes every line end in CRLF: a bare LF or CR becomes CRLF.
func canonicalCRLF(b []byte) []byte {
	if !bytes.ContainsAny(b, "\r\n") {
		return b
	}
	out := make([]byte, 0, len(b)+len(b)/32)
	for i := 0; i < len(b); i++ {
		switch c := b[i]; {
		case c == '\r' && i+1 < len(b) && b[i+1] == '\n':
			out = append(out, '\r', '\n')
			i++
		case c == '\r', c == '\n':
			out = append(out, '\r', '\n')
		default:
			out = append(out, c)
		}
	}
	return out
}

func trimCRLF(b []byte) []byte { return bytes.TrimRight(b, "\r\n") }

// splitHeader splits a CRLF message into its header fields (each with its
// folded lines and final CRLF) and its body, after the blank line.
func splitHeader(raw []byte) (fields [][]byte, body []byte) {
	rest := raw
	for len(rest) > 0 {
		end := bytes.Index(rest, []byte("\r\n"))
		if end == 0 { // the blank line
			return fields, rest[2:]
		}
		line := rest
		if end > 0 {
			line = rest[:end+2]
		}
		if (line[0] == ' ' || line[0] == '\t') && len(fields) > 0 {
			fields[len(fields)-1] = append(fields[len(fields)-1], line...)
		} else {
			fields = append(fields, append([]byte(nil), line...))
		}
		if end < 0 { // a header without a body or a final CRLF
			return fields, nil
		}
		rest = rest[end+2:]
	}
	return fields, nil
}

func fieldName(f []byte) string {
	name, _, _ := strings.Cut(string(f), ":")
	return strings.TrimSpace(name)
}

// escapeFrom keeps a line of a quoted-printable part from starting with
// "From ", which mbox delivery rewrites to ">From " and so breaks the
// signature: the F is written =46 and the line continues after a soft break,
// keeping it under 76 characters. Build writes every text part
// quoted-printable; an entity with a 7bit or 8bit part is left alone.
func escapeFrom(e []byte) []byte {
	lower := bytes.ToLower(e)
	if !bytes.Contains(e, []byte("\r\nFrom ")) || bytes.Contains(lower, []byte("content-transfer-encoding: 7bit")) ||
		bytes.Contains(lower, []byte("content-transfer-encoding: 8bit")) {
		return e
	}
	return bytes.ReplaceAll(e, []byte("\r\nFrom "), []byte("\r\n=46rom=\r\n "))
}

// sevenBit checks the entity survives any transport unchanged: ASCII
// without NUL, lines of at most 998 octets.
func sevenBit(e []byte) error {
	line := 0
	for _, c := range e {
		switch {
		case c == 0 || c > 127:
			return errors.New("openpgp: the message is not 7-bit clean")
		case c == '\n':
			line = 0
		default:
			if line++; line > 998 {
				return errors.New("openpgp: the message has a line over 998 characters")
			}
		}
	}
	return nil
}

// newBoundary is a boundary that occurs nowhere in e.
func newBoundary(e []byte) (string, error) {
	for range 8 {
		var rnd [18]byte
		if _, err := rand.Read(rnd[:]); err != nil {
			return "", err
		}
		b := "pgp-" + hex.EncodeToString(rnd[:])
		if !bytes.Contains(e, []byte(b)) {
			return b, nil
		}
	}
	return "", errors.New("openpgp: no free boundary")
}

// PGPMIME is the OpenPGP part found in a received message.
type PGPMIME struct {
	// Kind is "signed" (multipart/signed with application/pgp-signature) or
	// "encrypted" (multipart/encrypted with application/pgp-encrypted).
	Kind string
	// Signed is the signed entity, byte for byte as signed (CRLF lines),
	// and Signature its detached signature.
	Signed, Signature []byte
	// Encrypted is the OpenPGP message of an encrypted one.
	Encrypted []byte
	// Whole: it is the message itself, not one part of it among others (a
	// mailing list's footer added after signing, say).
	Whole bool
}

// FindPGPMIME returns the first PGP/MIME signed or encrypted entity of raw,
// the message or a part of it, depth first; nil when there is none. Lines
// are made CRLF first, the form the signature was made over.
func FindPGPMIME(raw []byte) (*PGPMIME, error) {
	return findPGPMIME(canonicalCRLF(raw), 0)
}

func findPGPMIME(e []byte, depth int) (*PGPMIME, error) {
	if depth > MaxMIMEDepth {
		return nil, nil
	}
	fields, body := splitHeader(e)
	ct, params := contentType(fields)
	if !strings.HasPrefix(ct, "multipart/") || params["boundary"] == "" {
		return nil, nil
	}
	parts := splitMultipart(body, params["boundary"])
	protocol := strings.ToLower(params["protocol"])
	switch {
	case ct == "multipart/signed" && protocol == "application/pgp-signature":
		if len(parts) != 2 {
			return nil, fmt.Errorf("openpgp: a signed message has %d parts, not 2", len(parts))
		}
		sf, sb := splitHeader(parts[1])
		if t, _ := contentType(sf); t != "application/pgp-signature" {
			return nil, fmt.Errorf("openpgp: the signature part is %s", t)
		}
		sig, err := DecodePart(sb, headerValue(sf, "Content-Transfer-Encoding"), "")
		if err != nil {
			return nil, err
		}
		return &PGPMIME{Kind: "signed", Signed: parts[0], Signature: sig, Whole: depth == 0}, nil
	case ct == "multipart/encrypted" && protocol == "application/pgp-encrypted":
		if len(parts) != 2 {
			return nil, fmt.Errorf("openpgp: an encrypted message has %d parts, not 2", len(parts))
		}
		ef, eb := splitHeader(parts[1])
		data, err := DecodePart(eb, headerValue(ef, "Content-Transfer-Encoding"), "")
		if err != nil {
			return nil, err
		}
		return &PGPMIME{Kind: "encrypted", Encrypted: data, Whole: depth == 0}, nil
	}
	for _, p := range parts {
		if found, err := findPGPMIME(p, depth+1); found != nil || err != nil {
			return found, err
		}
	}
	return nil, nil
}

// contentType is the entity's media type, lower-cased, and its parameters;
// text/plain when it has none.
func contentType(fields [][]byte) (string, map[string]string) {
	v := headerValue(fields, "Content-Type")
	if v == "" {
		return "text/plain", map[string]string{}
	}
	t, params, err := mime.ParseMediaType(v)
	if err != nil {
		t, _, _ = strings.Cut(v, ";")
		return strings.ToLower(strings.TrimSpace(t)), map[string]string{}
	}
	return t, params
}

// headerValue is the unfolded value of the first field named name.
func headerValue(fields [][]byte, name string) string {
	for _, f := range fields {
		if strings.EqualFold(fieldName(f), name) {
			_, v, _ := strings.Cut(string(f), ":")
			return strings.TrimSpace(strings.NewReplacer("\r\n", "", "\n", "").Replace(v))
		}
	}
	return ""
}

// splitMultipart returns the parts of a multipart body, each exactly as
// written between its delimiter line and the CRLF before the next one.
func splitMultipart(body []byte, boundary string) [][]byte {
	delim := []byte("--" + boundary)
	var parts [][]byte
	start := -1 // where the current part begins; -1 in the preamble
	for i := 0; i < len(body); {
		atLine := i == 0 || (i >= 2 && body[i-2] == '\r' && body[i-1] == '\n')
		if !atLine || !bytes.HasPrefix(body[i:], delim) {
			next := bytes.Index(body[i:], []byte("\r\n"))
			if next < 0 {
				break
			}
			i += next + 2
			continue
		}
		after := body[i+len(delim):]
		closing := bytes.HasPrefix(after, []byte("--"))
		rest := after
		if closing {
			rest = after[2:]
		}
		eol := bytes.Index(rest, []byte("\r\n"))
		pad := rest
		if eol >= 0 {
			pad = rest[:eol]
		}
		if len(bytes.Trim(pad, " \t")) != 0 { // another boundary this one is a prefix of
			i += len(delim)
			continue
		}
		if start >= 0 {
			end := i
			if end >= 2 {
				end -= 2 // the CRLF before the delimiter belongs to it
			}
			parts = append(parts, body[start:max(end, start)])
		}
		if closing || eol < 0 {
			return parts
		}
		i += len(delim) + (len(after) - len(rest)) + eol + 2
		start = i
	}
	return parts
}

// Inline OpenPGP armor in a text part (not PGP/MIME).
const (
	armorSigned       = "-----BEGIN PGP SIGNED MESSAGE-----"
	armorSignatureEnd = "-----END PGP SIGNATURE-----"
	armorMessage      = "-----BEGIN PGP MESSAGE-----"
	armorMessageEnd   = "-----END PGP MESSAGE-----"
)

// InlinePGP is an inline OpenPGP block of a text/plain part.
type InlinePGP struct {
	// Kind is "signed" (a cleartext signature) or "message" (an OpenPGP
	// message, usually encrypted).
	Kind string
	// Block is the armored block in the part's own charset, from its BEGIN
	// line to its END line, CRLF lines.
	Block   []byte
	Charset string // the part's charset, for the text a block holds
	// Whole: the block is all the message's text, nothing around it.
	Whole bool
}

// FindInlinePGP returns the first inline OpenPGP block in a text/plain part
// of raw that is not an attachment; nil when there is none.
func FindInlinePGP(raw []byte) (*InlinePGP, error) {
	return findInline(canonicalCRLF(raw), 0)
}

func findInline(e []byte, depth int) (*InlinePGP, error) {
	if depth > MaxMIMEDepth {
		return nil, nil
	}
	fields, body := splitHeader(e)
	ct, params := contentType(fields)
	if strings.HasPrefix(ct, "multipart/") {
		if params["boundary"] == "" {
			return nil, nil
		}
		for _, p := range splitMultipart(body, params["boundary"]) {
			if found, err := findInline(p, depth+1); found != nil || err != nil {
				return found, err
			}
		}
		return nil, nil
	}
	disp, _, _ := mime.ParseMediaType(headerValue(fields, "Content-Disposition"))
	if ct != "text/plain" || disp == "attachment" {
		return nil, nil
	}
	text, err := DecodePart(body, headerValue(fields, "Content-Transfer-Encoding"), "")
	if err != nil {
		return nil, err
	}
	text = canonicalCRLF(text)
	for _, k := range []struct{ kind, begin, end string }{{"signed", armorSigned, armorSignatureEnd}, {"message", armorMessage, armorMessageEnd}} {
		if b := armorBlock(text, k.begin, k.end); b != nil {
			whole := depth == 0 && bytes.Equal(bytes.TrimSpace(b), bytes.TrimSpace(text))
			return &InlinePGP{Kind: k.kind, Block: b, Charset: params["charset"], Whole: whole}, nil
		}
	}
	return nil, nil
}

// armorBlock is text from a line that is begin to the end of a later line
// that is end, CRLF included; nil when there is none.
func armorBlock(text []byte, begin, end string) []byte {
	i := bytes.Index(text, []byte(begin))
	for i > 0 && text[i-1] != '\n' {
		next := bytes.Index(text[i+1:], []byte(begin))
		if next < 0 {
			return nil
		}
		i += 1 + next
	}
	if i < 0 {
		return nil
	}
	j := bytes.Index(text[i:], []byte("\r\n"+end))
	if j < 0 {
		return nil
	}
	stop := i + j + 2 + len(end)
	if eol := bytes.Index(text[stop:], []byte("\r\n")); eol >= 0 {
		stop += eol + 2
	} else {
		stop = len(text)
	}
	return text[i:stop]
}
