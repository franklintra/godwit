package gpg

import (
	"bytes"
	"strconv"
	"strings"
	"time"
)

// Status is one of gpg's status lines: "[GNUPG:] KEYWORD arg…" (doc/DETAILS
// in GnuPG's source).
type Status struct {
	Keyword string
	Args    []string
}

func (s Status) arg(i int) string {
	if i < len(s.Args) {
		return s.Args[i]
	}
	return ""
}

const statusPrefix = "[GNUPG:] "

// splitStatus separates gpg's stderr into its status lines and the rest,
// its human messages.
func splitStatus(stderr []byte) ([]Status, string) {
	var out []Status
	var rest strings.Builder
	for line := range bytes.Lines(stderr) {
		l := strings.TrimRight(string(line), "\r\n")
		if s, ok := strings.CutPrefix(l, statusPrefix); ok {
			f := strings.Fields(s)
			if len(f) > 0 {
				out = append(out, Status{Keyword: f[0], Args: f[1:]})
			}
			continue
		}
		rest.WriteString(l)
		rest.WriteByte('\n')
	}
	return out, rest.String()
}

// ParseStatus reads the status lines of gpg's output, ignoring other lines.
func ParseStatus(stderr []byte) []Status {
	s, _ := splitStatus(stderr)
	return s
}

// SigStatus is what gpg found of one signature.
type SigStatus string

const (
	SigGood         SigStatus = "good"          // GOODSIG: it verifies; see Trust for whose it is
	SigBad          SigStatus = "bad"           // BADSIG: the data or the signature was changed
	SigExpired      SigStatus = "expired"       // EXPSIG: good, but the signature has expired
	SigExpiredKey   SigStatus = "expired_key"   // EXPKEYSIG: good, made by a key that has expired
	SigRevokedKey   SigStatus = "revoked_key"   // REVKEYSIG: good, made by a key that was revoked
	SigNoPublicKey  SigStatus = "no_public_key" // ERRSIG with NO_PUBKEY: the signer's key is not in the keyring
	SigError        SigStatus = "error"         // ERRSIG for any other reason (an unsupported algorithm…)
	SigUnknownState SigStatus = ""
)

// Trust is the validity gpg gives the signer's key and user ID under the
// user's trust model (TRUST_* status lines), TOFU and the web of trust alike.
type Trust string

const (
	TrustUndefined Trust = "undefined"
	TrustNever     Trust = "never"
	TrustMarginal  Trust = "marginal"
	TrustFully     Trust = "fully"
	TrustUltimate  Trust = "ultimate"
)

// Valid reports whether the trust model vouches for the key: marginal or better.
func (t Trust) Valid() bool { return t == TrustMarginal || t == TrustFully || t == TrustUltimate }

// Signature is one signature gpg checked.
type Signature struct {
	Status SigStatus
	KeyID  string // the long key ID the signature names
	UserID string // the key's primary user ID as gpg printed it (GOODSIG and the like)
	// Fingerprint is the signing (sub)key's, PrimaryFingerprint its primary
	// key's (VALIDSIG); both empty without the public key.
	Fingerprint, PrimaryFingerprint string
	Created                         time.Time
	HashAlgo                        int    // OpenPGP hash algorithm ID, 8 for SHA-256
	Trust                           Trust  // "" when gpg reported none (a bad signature)
	TrustModel                      string // pgp, tofu…, as gpg named it
}

// Result is what one verify or decrypt run reported.
type Result struct {
	Signatures []Signature
	// Encrypted: the input was encrypted. Decrypted: gpg decrypted it and
	// its integrity check passed (DECRYPTION_OKAY without a failure).
	Encrypted, Decrypted bool
	// NoSecretKey lists the key IDs the message was encrypted to whose
	// secret key is missing (NO_SECKEY); EncryptedTo every key ID (ENC_TO).
	NoSecretKey, EncryptedTo []string
}

// ParseResult reads signatures and decryption from status lines.
func ParseResult(status []Status) Result {
	var r Result
	var cur *Signature
	newSig := func() *Signature {
		r.Signatures = append(r.Signatures, Signature{})
		return &r.Signatures[len(r.Signatures)-1]
	}
	sig := func() *Signature {
		if cur == nil {
			cur = newSig()
		}
		return cur
	}
	decryptionFailed := false
	for _, s := range status {
		switch s.Keyword {
		case "NEWSIG":
			cur = newSig()
		case "GOODSIG", "BADSIG", "EXPSIG", "EXPKEYSIG", "REVKEYSIG":
			g := sig()
			g.Status = map[string]SigStatus{"GOODSIG": SigGood, "BADSIG": SigBad, "EXPSIG": SigExpired,
				"EXPKEYSIG": SigExpiredKey, "REVKEYSIG": SigRevokedKey}[s.Keyword]
			g.KeyID = s.arg(0)
			g.UserID = unescapeStatus(strings.Join(s.Args[min(1, len(s.Args)):], " "))
		case "ERRSIG":
			// ERRSIG <keyid> <pkalgo> <hashalgo> <sig_class> <time> <rc> [<fpr>]
			g := sig()
			g.KeyID = s.arg(0)
			g.HashAlgo, _ = strconv.Atoi(s.arg(2))
			g.Created = statusTime(s.arg(4))
			if s.arg(5) == "9" {
				g.Status = SigNoPublicKey
			} else if g.Status == SigUnknownState {
				g.Status = SigError
			}
			if fpr := s.arg(6); fpr != "" && fpr != "-" {
				g.Fingerprint = fpr
			}
		case "NO_PUBKEY":
			if cur != nil {
				cur.Status = SigNoPublicKey
			}
		case "VALIDSIG":
			// VALIDSIG <fpr> <date> <timestamp> <expire> <version> <reserved> <pkalgo> <hashalgo> <class> [<primary-fpr>]
			g := sig()
			g.Fingerprint = s.arg(0)
			g.Created = statusTime(s.arg(2))
			g.HashAlgo, _ = strconv.Atoi(s.arg(7))
			g.PrimaryFingerprint = s.arg(9)
			if g.PrimaryFingerprint == "" {
				g.PrimaryFingerprint = g.Fingerprint
			}
		case "TRUST_UNDEFINED", "TRUST_NEVER", "TRUST_MARGINAL", "TRUST_FULLY", "TRUST_ULTIMATE":
			g := sig()
			g.Trust = Trust(strings.ToLower(strings.TrimPrefix(s.Keyword, "TRUST_")))
			g.TrustModel = s.arg(1)
		case "BEGIN_DECRYPTION", "ENC_TO", "DECRYPTION_INFO":
			r.Encrypted = true
			if s.Keyword == "ENC_TO" {
				r.EncryptedTo = append(r.EncryptedTo, s.arg(0))
			}
		case "NO_SECKEY":
			r.Encrypted = true
			r.NoSecretKey = append(r.NoSecretKey, s.arg(0))
		case "DECRYPTION_OKAY":
			r.Decrypted = true
		case "DECRYPTION_FAILED", "BADMDC":
			decryptionFailed = true
		}
	}
	if decryptionFailed {
		r.Decrypted = false
	}
	// A NEWSIG with nothing after it (gpg stopped) is no signature.
	kept := r.Signatures[:0]
	for _, g := range r.Signatures {
		if g.Status != SigUnknownState {
			kept = append(kept, g)
		}
	}
	r.Signatures = kept
	return r
}

// statusTime reads a status line's time: seconds since the epoch, or an ISO
// 8601 "20261126T143524".
func statusTime(s string) time.Time {
	if n, err := strconv.ParseInt(s, 10, 64); err == nil && n > 0 {
		return time.Unix(n, 0)
	}
	if t, err := time.Parse("20060102T150405", s); err == nil {
		return t
	}
	return time.Time{}
}

// unescapeStatus decodes the %XX escapes of a status line's free text.
func unescapeStatus(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+3], 16, 8); err == nil {
				b.WriteByte(byte(n))
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// hashNames are the RFC 3156 micalg names of OpenPGP hash algorithm IDs.
var hashNames = map[int]string{1: "pgp-md5", 2: "pgp-sha1", 3: "pgp-ripemd160", 8: "pgp-sha256", 9: "pgp-sha384", 10: "pgp-sha512", 11: "pgp-sha224"}

// Micalg is the micalg parameter of multipart/signed for hash algorithm id,
// "" for one RFC 3156 has no name for.
func Micalg(id int) string { return hashNames[id] }
