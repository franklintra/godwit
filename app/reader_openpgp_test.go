package app

import (
	"errors"
	"testing"

	"github.com/godwit-email/godwit/core"
	"github.com/godwit-email/godwit/internal/i18n"
)

func TestOpenPGPSummary(t *testing.T) {
	sig := func(status, detail string, matches bool) *core.OpenPGPSignature {
		return &core.OpenPGPSignature{Status: status, Detail: detail, UserID: "Bob Example <bob@example.com>", KeyID: "0123456789ABCDEF",
			Fingerprint: "AAAABBBBCCCCDDDDEEEEFFFF0000111122223333", Trust: "marginal", TrustModel: "tofu", MatchesSender: matches}
	}
	for _, c := range []struct {
		name       string
		st         openpgpState
		text, icon string
	}{
		{"none", openpgpState{view: &core.OpenPGPView{}}, "", ""},
		{"gpg failed", openpgpState{err: errors.New("boom")}, i18n.T("reader.openpgp.error"), "shield"},
		{"verified", openpgpState{view: &core.OpenPGPView{Kind: "signed", Signature: sig(core.OpenPGPVerified, "good", true)}},
			i18n.Tf("reader.openpgp.verified", "Bob Example"), "shield-check"},
		{"not the sender", openpgpState{view: &core.OpenPGPView{Kind: "signed", Signature: sig(core.OpenPGPUnverified, "good", false)}},
			i18n.Tf("reader.openpgp.not_sender", "Bob Example"), "shield"},
		{"uncertified", openpgpState{view: &core.OpenPGPView{Kind: "signed", Signature: sig(core.OpenPGPUnverified, "good", true)}},
			i18n.Tf("reader.openpgp.unverified", "Bob Example"), "shield"},
		{"expired key", openpgpState{view: &core.OpenPGPView{Kind: "signed", Signature: sig(core.OpenPGPUnverified, "expired_key", true)}},
			i18n.Tf("reader.openpgp.key_expired", "Bob Example"), "shield"},
		{"bad", openpgpState{view: &core.OpenPGPView{Kind: "signed", Signature: sig(core.OpenPGPBad, "bad", true)}},
			i18n.T("reader.openpgp.bad"), "spam"},
		{"unknown key", openpgpState{view: &core.OpenPGPView{Kind: "signed", Signature: &core.OpenPGPSignature{Status: core.OpenPGPUnknownKey, KeyID: "0123456789ABCDEF"}}},
			i18n.Tf("reader.openpgp.unknown_key", "0123456789ABCDEF"), "shield"},
		{"decrypted and signed", openpgpState{view: &core.OpenPGPView{Kind: "encrypted", Decrypted: true, Signature: sig(core.OpenPGPVerified, "good", true)}},
			i18n.T("reader.openpgp.decrypted") + " · " + i18n.Tf("reader.openpgp.verified", "Bob Example"), "shield-check"},
		{"not for me", openpgpState{view: &core.OpenPGPView{Kind: "encrypted", DecryptError: "no_secret_key"}},
			i18n.T("reader.openpgp.no_secret_key"), "lock"},
	} {
		text, tip, icon := openpgpSummary(c.st)
		if text != c.text || icon != c.icon {
			t.Errorf("%s: %q %q, want %q %q", c.name, text, icon, c.text, c.icon)
		}
		if c.st.view != nil && c.st.view.Signature != nil && c.st.view.Signature.Fingerprint != "" && tip == "" {
			t.Errorf("%s: the tooltip names the key", c.name)
		}
	}
}

func TestGroupFingerprint(t *testing.T) {
	if got := groupFingerprint("AAAABBBBCC"); got != "AAAA BBBB CC" {
		t.Errorf("got %q", got)
	}
}
