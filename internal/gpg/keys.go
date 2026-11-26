package gpg

import (
	"context"
	"net/mail"
	"strconv"
	"strings"
	"time"
)

// Key is one key of a --with-colons listing.
type Key struct {
	Fingerprint string // the primary key's
	KeyID       string // the primary key's long key ID
	// Validity is gpg's validity letter for the key (field 2): u ultimate,
	// f full, m marginal, - or q unknown, n never, e expired, r revoked, d
	// disabled, i invalid.
	Validity string
	// Capabilities are the usable capabilities of the whole key, primary and
	// subkeys together (the primary line's upper-case letters): S sign, E
	// encrypt, C certify, A authenticate, D disabled.
	Capabilities string
	Algorithm    string // the curve, or the algorithm and its size, e.g. "ed25519", "rsa4096"
	Created      time.Time
	Expires      time.Time // zero: never
	Secret       bool      // a secret key listing, its secret part present (not a stub)
	UserIDs      []UserID
}

// UserID is a key's user ID with the validity gpg gives it.
type UserID struct {
	Text     string // as on the key, "Name <addr>"
	Email    string // its address, lower-cased; "" when it has none
	Validity string // as Key.Validity
}

// unusable validities: the key or user ID must not be used at all.
func unusable(v string) bool { return v == "e" || v == "r" || v == "d" || v == "i" }

// validLetter: the trust model vouches for it (marginal or better).
func validLetter(v string) bool { return v == "m" || v == "f" || v == "u" }

// CanSign reports whether the key can make signatures now.
func (k Key) CanSign() bool {
	return !unusable(k.Validity) && strings.Contains(k.Capabilities, "S") && !strings.Contains(k.Capabilities, "D")
}

// EncryptsTo reports whether the key can be encrypted to for addr under the
// user's trust model: the key can encrypt, and its user ID with that address
// is valid (marginal or better) and neither revoked nor expired.
func (k Key) EncryptsTo(addr string) bool {
	if unusable(k.Validity) || !strings.Contains(k.Capabilities, "E") || strings.Contains(k.Capabilities, "D") {
		return false
	}
	addr = strings.ToLower(addr)
	for _, u := range k.UserIDs {
		if u.Email == addr && validLetter(u.Validity) {
			return true
		}
	}
	return false
}

// HasEmail reports whether one of the key's user IDs, valid or not, has addr.
func (k Key) HasEmail(addr string) bool {
	addr = strings.ToLower(addr)
	for _, u := range k.UserIDs {
		if u.Email == addr && !unusable(u.Validity) {
			return true
		}
	}
	return false
}

// SecretKeys lists the user's secret keys.
func (g *GPG) SecretKeys(ctx context.Context) ([]Key, error) {
	out, _, _, err := g.run(ctx, "list-secret-keys", QuickTimeout, nil, nil, "--list-secret-keys")
	if err != nil {
		return nil, err
	}
	return ParseKeys(out), nil
}

// PublicKeys lists the public keys matching any of patterns (all keys when
// there is none). A pattern that matches nothing is not an error.
func (g *GPG) PublicKeys(ctx context.Context, patterns ...string) ([]Key, error) {
	args := []string{"--list-keys"}
	if len(patterns) > 0 {
		args = append(args, "--")
		args = append(args, patterns...)
	}
	// gpg exits 2 when a pattern matches no key, after listing the others.
	out, _, _, err := g.run(ctx, "list-keys", QuickTimeout, nil, func(code int) bool { return code == 2 }, args...)
	if err != nil {
		return nil, err
	}
	return ParseKeys(out), nil
}

// ParseKeys reads a --with-colons --fixed-list-mode key listing.
func ParseKeys(out []byte) []Key {
	var keys []Key
	var cur *Key
	primaryFpr := false // the next fpr line is the primary key's
	for line := range strings.Lines(string(out)) {
		f := strings.Split(strings.TrimRight(line, "\r\n"), ":")
		field := func(i int) string { // 1-based, as in GnuPG's doc/DETAILS
			if i-1 < len(f) {
				return f[i-1]
			}
			return ""
		}
		switch field(1) {
		case "pub", "sec":
			keys = append(keys, Key{
				KeyID: field(5), Validity: field(2), Capabilities: upper(field(12)),
				Algorithm: algorithm(field(4), field(3), field(17)),
				Created:   colonTime(field(6)), Expires: colonTime(field(7)),
				Secret: field(1) == "sec" && field(15) != "#",
			})
			cur, primaryFpr = &keys[len(keys)-1], true
		case "fpr":
			if cur != nil && primaryFpr {
				cur.Fingerprint = field(10)
			}
			primaryFpr = false
		case "sub", "ssb":
			primaryFpr = false
		case "uid":
			if cur != nil {
				text := unescapeColons(field(10))
				cur.UserIDs = append(cur.UserIDs, UserID{Text: text, Email: emailOf(text), Validity: field(2)})
			}
		}
	}
	return keys
}

// upper keeps the upper-case letters: the whole key's usable capabilities.
func upper(caps string) string {
	var b strings.Builder
	for _, r := range caps {
		if r >= 'A' && r <= 'Z' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// algorithm names a key's algorithm: its curve when it has one.
func algorithm(algo, bits, curve string) string {
	if curve != "" {
		return curve
	}
	switch algo {
	case "1", "2", "3":
		return "rsa" + bits
	case "17":
		return "dsa" + bits
	case "16", "20":
		return "elgamal" + bits
	}
	return "algo" + algo
}

func colonTime(s string) time.Time {
	if n, err := strconv.ParseInt(s, 10, 64); err == nil && n > 0 {
		return time.Unix(n, 0)
	}
	if t, err := time.Parse("20060102T150405", s); err == nil {
		return t
	}
	return time.Time{}
}

// unescapeColons decodes a --with-colons field's \xHH escapes.
func unescapeColons(s string) string {
	if !strings.Contains(s, `\x`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) && s[i+1] == 'x' {
			if n, err := strconv.ParseUint(s[i+2:i+4], 16, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// emailOf is the lower-cased address of a user ID: "Name <addr>", "<addr>"
// or a bare address; "" when it has none.
func emailOf(uid string) string {
	if a, err := mail.ParseAddress(uid); err == nil {
		return strings.ToLower(a.Address)
	}
	if i, j := strings.LastIndexByte(uid, '<'), strings.LastIndexByte(uid, '>'); i >= 0 && j > i {
		uid = uid[i+1 : j]
	}
	if uid = strings.TrimSpace(uid); strings.Count(uid, "@") == 1 && !strings.ContainsAny(uid, " <>") {
		return strings.ToLower(uid)
	}
	return ""
}
