package app

import (
	"cmp"
	"context"
	netmail "net/mail"
	"strings"

	"github.com/egoist/mygo/ui"
	"github.com/godwit-email/godwit/core"
	"github.com/godwit-email/godwit/internal/i18n"
	"github.com/godwit-email/godwit/internal/kit"
)

// identityForm is the identity dialog's fields: a new identity (id 0) or
// identity id's.
type identityForm struct {
	id                   int64
	name, email, replyTo string
	signature            int64 // 0: the account's
	isDefault            bool
}

// identityLabel is how a list shows the identity: its From address.
func identityLabel(acc core.Account, i core.Identity) string {
	return (&netmail.Address{Name: cmp.Or(i.Name, acc.Name), Address: i.Email}).String()
}

// identitiesSection is an account's 发件身份 (ADR 0045): the other names
// and addresses it sends as, each with its edit and remove buttons, and
// the button that adds one.
func (p *settingsPage) identitiesSection(c *ui.Context, acc core.Account) {
	a := p.a
	th := c.Theme()
	var rows []func()
	for _, i := range a.accountIdentities(acc.ID) {
		var desc []string
		if i.IsDefault {
			desc = append(desc, i18n.T("settings.identity.default"))
		}
		if i.ReplyTo != "" {
			desc = append(desc, i18n.Tf("settings.identity.reply_to.row", i.ReplyTo))
		}
		if i.SignatureID != 0 {
			for _, s := range a.signatures {
				if s.ID == i.SignatureID {
					desc = append(desc, i18n.Tf("settings.identity.signature.row", s.Name))
				}
			}
		}
		rows = append(rows, func() {
			settingRow(c, identityLabel(acc, i), strings.Join(desc, " · "), func() ui.Element {
				ui.Row(c).Gap(th.Space(2)).AlignItems(ui.Center).Children(func() {
					rowButton(c, i18n.T("settings.identity.edit"), kit.Outline, false).OnClick(func() { p.openIdentity(acc, i) })
					kit.Button(c, i18n.T("settings.identity.remove"), kit.ButtonOpts{Size: kit.SizeSm, Variant: kit.Outline, Tone: kit.ToneDanger}).
						Label(i18n.Tf("settings.identity.remove.aria", identityLabel(acc, i))).OnClick(func() {
						p.openDialog("identity_remove", acc)
						p.identity = identityForm{id: i.ID, name: i.Name, email: i.Email}
					})
				})
				return ui.Element{}
			})
		})
	}
	rows = append(rows, func() {
		settingRow(c, i18n.T("settings.identity.add.row"), i18n.T("settings.identity.add.help"), func() ui.Element {
			rowButton(c, i18n.T("settings.identity.add"), kit.Outline, false).OnClick(func() { p.openIdentity(acc, core.Identity{}) })
			return ui.Element{}
		})
	})
	settingsSection(c, i18n.T("settings.identity"), i18n.T("settings.identity.foot"), rows...)
}

// openIdentity opens the identity dialog on i (ID 0: a new one).
func (p *settingsPage) openIdentity(acc core.Account, i core.Identity) {
	p.openDialog("identity", acc)
	p.identity = identityForm{id: i.ID, name: i.Name, email: i.Email, replyTo: i.ReplyTo, signature: i.SignatureID, isDefault: i.IsDefault}
}

// identityDialog is the identity dialogs' title, body and confirm action:
// "identity" adds or edits one, "identity_remove" asks before removing it.
func (p *settingsPage) identityDialog(c *ui.Context, acc core.Account) (title, desc, label string, opts kit.ButtonOpts, body, confirm func(), incomplete bool) {
	a, f, id := p.a, &p.identity, acc.ID
	label = i18n.T("common.save")
	if p.dialog == "identity_remove" {
		title = i18n.Tf("settings.identity.remove.title", identityLabel(acc, core.Identity{Name: f.name, Email: f.email}))
		desc = i18n.T("settings.identity.remove.help")
		label, opts = i18n.T("common.delete"), kit.ButtonOpts{Variant: kit.Ghost, Tone: kit.ToneDanger}
		ident := f.id
		confirm = func() {
			p.act(id, func(ctx context.Context) error { return a.svc.RemoveIdentity(ctx, ident) }, nil)
		}
		return
	}
	title = i18n.T("settings.identity.add.title")
	if f.id != 0 {
		title = i18n.T("settings.identity.edit.title")
	}
	desc = i18n.T("settings.identity.help")
	sigs := make([]kit.Option[int64], 0, len(a.signatures)+1)
	sigs = append(sigs, kit.Option[int64]{Value: 0, Label: i18n.T("settings.identity.signature.account")})
	for _, s := range a.signatures {
		sigs = append(sigs, kit.Option[int64]{Value: s.ID, Label: s.Name})
	}
	body = func() {
		th := c.Theme()
		ui.Column(c).Gap(th.Space(3)).Children(func() {
			formRow(c, i18n.T("settings.identity.email"), func() ui.Element {
				return kit.Input(c, &f.email, kit.InputOpts{Placeholder: "name@example.com"}).Grow(1)
			}, nil)
			formRow(c, i18n.T("settings.identity.name"), func() ui.Element {
				return kit.Input(c, &f.name, kit.InputOpts{Placeholder: acc.Name}).Grow(1) // empty: the account's name
			}, nil)
			formRow(c, i18n.T("settings.identity.reply_to"), func() ui.Element {
				return kit.Input(c, &f.replyTo, kit.InputOpts{}).Grow(1)
			}, nil)
			formRow(c, i18n.T("settings.identity.signature"), func() ui.Element {
				return kit.Select(c, &f.signature, sigs).Grow(1)
			}, nil)
			kit.FieldWith(c, kit.FieldOpts{Horizontal: true, Switch: &f.isDefault}, i18n.T("settings.identity.make_default"), i18n.T("settings.identity.make_default.help"), "", nil)
		})
	}
	incomplete = !strings.Contains(f.email, "@")
	in := core.IdentityInput{Name: f.name, Email: f.email, ReplyTo: f.replyTo, SignatureID: f.signature, IsDefault: f.isDefault}
	ident := f.id
	confirm = func() {
		p.act(id, func(ctx context.Context) error {
			if ident == 0 {
				_, err := a.svc.AddIdentity(ctx, id, in)
				return err
			}
			_, err := a.svc.UpdateIdentity(ctx, ident, core.IdentityPatch{Name: &in.Name, Email: &in.Email, ReplyTo: &in.ReplyTo,
				SignatureID: &in.SignatureID, IsDefault: &in.IsDefault})
			return err
		}, nil)
	}
	return
}
