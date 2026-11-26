package app

import (
	"context"
	"strings"

	"github.com/egoist/mygo/ui"
	"github.com/godwit-email/godwit/core"
	"github.com/godwit-email/godwit/internal/i18n"
	"github.com/godwit-email/godwit/internal/icons"
	"github.com/godwit-email/godwit/internal/kit"
	"github.com/godwit-email/godwit/internal/theme"
)

// openpgpState is what the user's gpg said of a message (ADR 0045): read
// once its body is, when the message looks signed or encrypted.
type openpgpState struct {
	view    *core.OpenPGPView
	err     error
	loading bool
}

// loadOpenPGP checks message id's signature and decrypts it through the
// user's gpg, tied to its body's read. The body's page decrypts its own copy
// (webui.Reader) from the same run: core reuses the result.
func (r *reader) loadOpenPGP(id int64) {
	a, b := r.p.a, r.bodies[id]
	if b == nil || core.OpenPGPKind(b.msg) == "" {
		return
	}
	b.openpgp = openpgpState{loading: true}
	var v core.OpenPGPView
	b.read.Also(a, func(ctx context.Context) (err error) {
		v, err = a.svc.OpenPGPMessage(ctx, id)
		return err
	}, func(err error) {
		b.openpgp = openpgpState{err: err}
		if err == nil {
			b.openpgp.view = &v
		}
	})
}

// openpgpNote is, in the header's second line, what OpenPGP says of the
// message: its signature and whether it was decrypted, the details in a tooltip.
func (r *reader) openpgpNote(c *ui.Context, b *msgBody) {
	if b == nil || !b.ready {
		return
	}
	st := b.openpgp
	t, th := theme.Of(c), c.Theme()
	if st.loading {
		kit.Text(c, "·", t.Text.Xs).TextColor(t.Muted)
		kit.Text(c, i18n.T("reader.openpgp.checking"), t.Text.Xs).TextColor(t.Muted)
		return
	}
	text, tip, icon := openpgpSummary(st)
	if text == "" {
		return
	}
	color := t.Muted
	switch icon {
	case "shield-check":
		color = t.Success
	case "spam":
		color = t.Danger
	}
	kit.Text(c, "·", t.Text.Xs).TextColor(t.Muted)
	note := ui.Row(c).Gap(th.Space(1)).AlignItems(ui.Center).TextColor(color).Label(text).Children(func() {
		ui.Icon(c, icons.Get(icon)).Size(theme.IconXs, theme.IconXs)
		kit.Text(c, text, t.Text.Xs)
	})
	if tip != "" {
		kit.Tooltip(c, note, tip)
	}
}

// openpgpSummary is the note's text, tooltip and icon for st; "" text for none.
func openpgpSummary(st openpgpState) (text, tip, icon string) {
	if st.err != nil {
		return i18n.T("reader.openpgp.error"), humanize(st.err), "shield"
	}
	v := st.view
	if v == nil || v.Kind == "" {
		return "", "", ""
	}
	var parts, tips []string
	icon = "shield"
	if v.Kind == "encrypted" {
		switch {
		case v.Decrypted:
			parts, icon = append(parts, i18n.T("reader.openpgp.decrypted")), "lock"
		case v.DecryptError == "no_secret_key":
			parts, icon = append(parts, i18n.T("reader.openpgp.no_secret_key")), "lock"
		default:
			parts, icon = append(parts, i18n.Tf("reader.openpgp.decrypt_failed", v.DecryptError)), "lock"
		}
	}
	if s := v.Signature; s != nil {
		who := signerName(s)
		switch s.Status {
		case core.OpenPGPVerified:
			parts, icon = append(parts, i18n.Tf("reader.openpgp.verified", who)), "shield-check"
		case core.OpenPGPUnverified:
			switch {
			case s.Detail == "expired_key" || s.Detail == "revoked_key" || s.Detail == "expired":
				parts = append(parts, i18n.Tf("reader.openpgp.key_expired", who))
			case !s.MatchesSender:
				parts = append(parts, i18n.Tf("reader.openpgp.not_sender", who))
			default:
				parts = append(parts, i18n.Tf("reader.openpgp.unverified", who))
			}
		case core.OpenPGPUnknownKey:
			parts = append(parts, i18n.Tf("reader.openpgp.unknown_key", s.KeyID))
		case core.OpenPGPBad:
			parts, icon = append(parts, i18n.T("reader.openpgp.bad")), "spam"
		default:
			parts = append(parts, i18n.T("reader.openpgp.error"))
		}
		if s.Fingerprint != "" {
			tips = append(tips, groupFingerprint(s.Fingerprint))
		}
		if s.Trust != "" {
			tips = append(tips, s.Trust+" ("+s.TrustModel+")")
		}
	}
	if v.Partial {
		tips = append(tips, i18n.T("reader.openpgp.partial"))
	}
	if len(parts) == 0 {
		return "", "", ""
	}
	return strings.Join(parts, " · "), strings.Join(tips, "\n"), icon
}

// signerName is how the note names a signer: their user ID's name, else
// the user ID, else the key ID.
func signerName(s *core.OpenPGPSignature) string {
	uid := strings.TrimSpace(s.UserID)
	if i := strings.Index(uid, " <"); i > 0 {
		return uid[:i]
	}
	if uid != "" {
		return uid
	}
	return s.KeyID
}

// groupFingerprint writes a fingerprint in groups of four, as gpg prints it.
func groupFingerprint(f string) string {
	var b strings.Builder
	for i := 0; i < len(f); i += 4 {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(f[i:min(i+4, len(f))])
	}
	return b.String()
}
