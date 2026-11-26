package gpg

import (
	"testing"
	"time"
)

// Recorded from gpg 2.4.8 (--status-fd 2): status lines among its messages.
const goodSig = `[GNUPG:] NEWSIG
gpg: Signature made Thu 26 Nov 2026 02:35:24 PM UTC
gpg:                using EDDSA key 591E8C89AF25FAA94A78FBC9611565E321B5CF36
[GNUPG:] KEY_CONSIDERED 591E8C89AF25FAA94A78FBC9611565E321B5CF36 0
[GNUPG:] SIG_ID /fFRUG57r26SPuuQSLrAlZa8yig 2026-11-26 1795703724
[GNUPG:] GOODSIG 611565E321B5CF36 Alice <alice@example.org>
gpg: Good signature from "Alice <alice@example.org>" [ultimate]
[GNUPG:] VALIDSIG 591E8C89AF25FAA94A78FBC9611565E321B5CF36 2026-11-26 1795703724 0 4 0 22 8 00 591E8C89AF25FAA94A78FBC9611565E321B5CF36
[GNUPG:] TRUST_ULTIMATE 0 pgp
`

const badSig = `[GNUPG:] NEWSIG
gpg: Signature made Thu 26 Nov 2026 02:35:24 PM UTC
[GNUPG:] KEY_CONSIDERED 591E8C89AF25FAA94A78FBC9611565E321B5CF36 0
[GNUPG:] BADSIG 611565E321B5CF36 Alice <alice@example.org>
gpg: BAD signature from "Alice <alice@example.org>" [ultimate]
[GNUPG:] FAILURE gpg-exit 33554433
`

const noPubkey = `[GNUPG:] NEWSIG
gpg: Signature made Thu 26 Nov 2026 02:35:24 PM UTC
[GNUPG:] ERRSIG 611565E321B5CF36 22 8 00 1795703724 9 591E8C89AF25FAA94A78FBC9611565E321B5CF36
[GNUPG:] NO_PUBKEY 611565E321B5CF36
gpg: Can't check signature: No public key
[GNUPG:] FAILURE gpg-exit 33554433
`

const tofuMarginal = `[GNUPG:] NEWSIG
[GNUPG:] KEY_CONSIDERED 591E8C89AF25FAA94A78FBC9611565E321B5CF36 0
[GNUPG:] SIG_ID /fFRUG57r26SPuuQSLrAlZa8yig 2026-11-26 1795703724
[GNUPG:] GOODSIG 611565E321B5CF36 Alice <alice@example.org>
[GNUPG:] VALIDSIG 591E8C89AF25FAA94A78FBC9611565E321B5CF36 2026-11-26 1795703724 0 4 0 22 8 00 591E8C89AF25FAA94A78FBC9611565E321B5CF36
[GNUPG:] TOFU_USER 591E8C89AF25FAA94A78FBC9611565E321B5CF36 alice@example.org
[GNUPG:] TOFU_STATS 2 1 0 auto 1795703724 1795703724 0 0 2 1 0
[GNUPG:] TOFU_STATS_LONG alice@example.org: Verified 1~signatures in the past 0~seconds.  Encrypted%0A0 messages.
gpg: Warning: we've only seen one message signed using this key and user id!
[GNUPG:] TRUST_MARGINAL 0 tofu
`

const decryptSigned = `[GNUPG:] ENC_TO 55A91767F35E7BC0 18 0
[GNUPG:] ENC_TO 153BBD444FF0F9D2 18 0
[GNUPG:] KEY_CONSIDERED 591E8C89AF25FAA94A78FBC9611565E321B5CF36 0
gpg: encrypted with cv25519 key, ID 153BBD444FF0F9D2, created 2026-11-26
      "Alice <alice@example.org>"
[GNUPG:] DECRYPTION_KEY A405676E3AC6350D954224F555A91767F35E7BC0 EE958E145920132962B3480E381D9B1B02BDCD81 u
[GNUPG:] NO_SECKEY 153BBD444FF0F9D2
[GNUPG:] BEGIN_DECRYPTION
[GNUPG:] DECRYPTION_INFO 2 9 0
[GNUPG:] PLAINTEXT 62 1795703724
[GNUPG:] PLAINTEXT_LENGTH 35
[GNUPG:] NEWSIG
[GNUPG:] KEY_CONSIDERED 591E8C89AF25FAA94A78FBC9611565E321B5CF36 0
[GNUPG:] SIG_ID 2ZyUFalJaE/GNSW6IYQcLHv+GaM 2026-11-26 1795703724
[GNUPG:] GOODSIG 611565E321B5CF36 Alice <alice@example.org>
[GNUPG:] VALIDSIG 591E8C89AF25FAA94A78FBC9611565E321B5CF36 2026-11-26 1795703724 0 4 0 22 10 00 591E8C89AF25FAA94A78FBC9611565E321B5CF36
[GNUPG:] TRUST_UNDEFINED 0 pgp
[GNUPG:] DECRYPTION_OKAY
[GNUPG:] GOODMDC
[GNUPG:] END_DECRYPTION
`

const decryptNoSeckey = `[GNUPG:] ENC_TO 55A91767F35E7BC0 18 0
[GNUPG:] ENC_TO 153BBD444FF0F9D2 18 0
gpg: encrypted with ECDH key, ID 153BBD444FF0F9D2
[GNUPG:] NO_SECKEY 153BBD444FF0F9D2
[GNUPG:] NO_SECKEY 55A91767F35E7BC0
gpg: public key decryption failed: No secret key
[GNUPG:] ERROR pkdecrypt_failed 33554449
[GNUPG:] BEGIN_DECRYPTION
[GNUPG:] DECRYPTION_FAILED
gpg: decryption failed: No secret key
[GNUPG:] END_DECRYPTION
[GNUPG:] FAILURE gpg-exit 33554433
`

func TestParseResultGood(t *testing.T) {
	r := ParseResult(ParseStatus([]byte(goodSig)))
	if len(r.Signatures) != 1 {
		t.Fatalf("signatures = %+v", r.Signatures)
	}
	g := r.Signatures[0]
	want := Signature{Status: SigGood, KeyID: "611565E321B5CF36", UserID: "Alice <alice@example.org>",
		Fingerprint: "591E8C89AF25FAA94A78FBC9611565E321B5CF36", PrimaryFingerprint: "591E8C89AF25FAA94A78FBC9611565E321B5CF36",
		Created: time.Unix(1795703724, 0), HashAlgo: 8, Trust: TrustUltimate, TrustModel: "pgp"}
	if g != want {
		t.Errorf("got  %+v\nwant %+v", g, want)
	}
	if r.Encrypted || r.Decrypted {
		t.Errorf("a signature is not encrypted: %+v", r)
	}
}

func TestParseResultBadAndMissing(t *testing.T) {
	r := ParseResult(ParseStatus([]byte(badSig)))
	if len(r.Signatures) != 1 || r.Signatures[0].Status != SigBad || r.Signatures[0].Trust != "" {
		t.Errorf("bad: %+v", r.Signatures)
	}
	r = ParseResult(ParseStatus([]byte(noPubkey)))
	if len(r.Signatures) != 1 || r.Signatures[0].Status != SigNoPublicKey || r.Signatures[0].KeyID != "611565E321B5CF36" ||
		r.Signatures[0].Fingerprint != "591E8C89AF25FAA94A78FBC9611565E321B5CF36" {
		t.Errorf("no pubkey: %+v", r.Signatures)
	}
}

func TestParseResultTOFU(t *testing.T) {
	r := ParseResult(ParseStatus([]byte(tofuMarginal)))
	if len(r.Signatures) != 1 {
		t.Fatalf("signatures = %+v", r.Signatures)
	}
	if g := r.Signatures[0]; g.Status != SigGood || g.Trust != TrustMarginal || g.TrustModel != "tofu" || !g.Trust.Valid() {
		t.Errorf("tofu: %+v", g)
	}
}

func TestParseResultDecrypt(t *testing.T) {
	r := ParseResult(ParseStatus([]byte(decryptSigned)))
	if !r.Encrypted || !r.Decrypted {
		t.Errorf("decrypted: %+v", r)
	}
	if len(r.Signatures) != 1 || r.Signatures[0].Status != SigGood || r.Signatures[0].Trust != TrustUndefined || r.Signatures[0].HashAlgo != 10 {
		t.Errorf("inner signature: %+v", r.Signatures)
	}
	if len(r.EncryptedTo) != 2 || len(r.NoSecretKey) != 1 {
		t.Errorf("keys: to %v, missing %v", r.EncryptedTo, r.NoSecretKey)
	}
	r = ParseResult(ParseStatus([]byte(decryptNoSeckey)))
	if !r.Encrypted || r.Decrypted || len(r.NoSecretKey) != 2 || len(r.Signatures) != 0 {
		t.Errorf("no seckey: %+v", r)
	}
}

func TestParseResultMDCFailure(t *testing.T) {
	r := ParseResult(ParseStatus([]byte("[GNUPG:] BEGIN_DECRYPTION\n[GNUPG:] DECRYPTION_OKAY\n[GNUPG:] BADMDC\n")))
	if r.Decrypted {
		t.Error("a failed integrity check must not count as decrypted")
	}
}

func TestSplitStatusKeepsMessages(t *testing.T) {
	st, msgs := splitStatus([]byte(badSig))
	if len(st) != 4 || st[2].Keyword != "BADSIG" {
		t.Errorf("status = %+v", st)
	}
	if lastLine(msgs) != `BAD signature from "Alice <alice@example.org>" [ultimate]` {
		t.Errorf("messages = %q", msgs)
	}
}

func TestUnescapeStatus(t *testing.T) {
	if got := unescapeStatus("Ali%25ce %0A<a@x>"); got != "Ali%ce \n<a@x>" {
		t.Errorf("got %q", got)
	}
}

func TestMicalg(t *testing.T) {
	for id, want := range map[int]string{8: "pgp-sha256", 10: "pgp-sha512", 2: "pgp-sha1", 99: ""} {
		if got := Micalg(id); got != want {
			t.Errorf("Micalg(%d) = %q, want %q", id, got, want)
		}
	}
}

const secretListing = `sec:u:255:22:611565E321B5CF36:1795703724:::u:::scESC:::+::ed25519:::0:
fpr:::::::::591E8C89AF25FAA94A78FBC9611565E321B5CF36:
grp:::::::::F2DD6115A87B07A49D847C6AD5BBAE42D7110A91:
uid:u::::1795703724::76BD147C838E2BFA15D0FED1963B6820282E9515::Alice <alice@example.org>::::::::::0:
uid:r::::1795703724::76BD147C838E2BFA15D0FED1963B6820282E9516::Alice Old <old@example.org>::::::::::0:
ssb:u:255:18:153BBD444FF0F9D2:1795703724::::::e:::+::cv25519::
fpr:::::::::2483625F39CB81BBD147A8B3153BBD444FF0F9D2:
grp:::::::::F033C8E1548EBF2BBEE49319EA580EDBA4F41B60:
sec:e:4096:1:0123456789ABCDEF:1500000000:1600000000::u:::sc:::#:::::0:
fpr:::::::::AAAA456789ABCDEF0123456789ABCDEF01234567:
uid:e::::1500000000::X::Expired \x3a colon <EXP@Example.org>::::::::::0:
`

const publicListing = `tru::1:1795703724:0:3:1:5
pub:-:255:22:381D9B1B02BDCD81:1795703724:::-:::scESC:::::ed25519:::0:
fpr:::::::::EE958E145920132962B3480E381D9B1B02BDCD81:
uid:-::::1795703724::CE9CE479EB13E4980263AEC97C359238FE05C607::Bob <bob@example.org>::::::::::0:
sub:-:255:18:55A91767F35E7BC0:1795703724::::::e:::::cv25519::
fpr:::::::::A405676E3AC6350D954224F555A91767F35E7BC0:
pub:f:255:22:381D9B1B02BDCD82:1795703724:::-:::scESC:::::ed25519:::0:
fpr:::::::::EE958E145920132962B3480E381D9B1B02BDCD82:
uid:f::::1795703724::CE9CE479EB13E4980263AEC97C359238FE05C608::Carol <Carol@Example.org>::::::::::0:
sub:f:255:18:55A91767F35E7BC1:1795703724::::::e:::::cv25519::
fpr:::::::::A405676E3AC6350D954224F555A91767F35E7BC1:
`

func TestParseKeys(t *testing.T) {
	keys := ParseKeys([]byte(secretListing))
	if len(keys) != 2 {
		t.Fatalf("keys = %+v", keys)
	}
	a := keys[0]
	if a.Fingerprint != "591E8C89AF25FAA94A78FBC9611565E321B5CF36" || a.KeyID != "611565E321B5CF36" || a.Algorithm != "ed25519" ||
		!a.Secret || a.Capabilities != "ESC" || !a.Expires.IsZero() || len(a.UserIDs) != 2 {
		t.Errorf("alice: %+v", a)
	}
	if !a.CanSign() || a.UserIDs[0].Email != "alice@example.org" {
		t.Errorf("alice signs as alice@example.org: %+v", a)
	}
	if a.HasEmail("old@example.org") {
		t.Error("a revoked user ID does not count")
	}
	e := keys[1]
	if e.CanSign() || e.Secret || e.Algorithm != "rsa4096" || e.Expires != time.Unix(1600000000, 0) {
		t.Errorf("expired stub: %+v", e)
	}
	if e.UserIDs[0].Text != "Expired : colon <EXP@Example.org>" || e.UserIDs[0].Email != "exp@example.org" {
		t.Errorf("uid: %+v", e.UserIDs[0])
	}

	pub := ParseKeys([]byte(publicListing))
	if len(pub) != 2 {
		t.Fatalf("public = %+v", pub)
	}
	if pub[0].EncryptsTo("bob@example.org") {
		t.Error("a key of unknown validity is not encrypted to")
	}
	if !pub[1].EncryptsTo("carol@example.ORG") || pub[1].EncryptsTo("bob@example.org") {
		t.Errorf("carol: %+v", pub[1])
	}
}

func TestEmailOf(t *testing.T) {
	for uid, want := range map[string]string{
		"Alice <Alice@Example.org>":      "alice@example.org",
		"<bob@example.org>":              "bob@example.org",
		"carol@example.org":              "carol@example.org",
		"Dave (work) <dave@example.org>": "dave@example.org",
		"Just a name":                    "",
		`"Odd, Name" <odd@example.org>`:  "odd@example.org",
	} {
		if got := emailOf(uid); got != want {
			t.Errorf("emailOf(%q) = %q, want %q", uid, got, want)
		}
	}
}
