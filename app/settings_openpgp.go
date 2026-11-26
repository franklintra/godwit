package app

import (
	"context"
	"strings"

	"github.com/egoist/mygo/ui"
	"github.com/godwit-email/godwit/core"
	"github.com/godwit-email/godwit/internal/i18n"
	"github.com/godwit-email/godwit/internal/kit"
)

// openpgpView is an account page's OpenPGP section (ADR 0045): the gpg
// Godwit runs, the account's signing key from the user's keyring, and its
// defaults for new drafts.
type openpgpView struct {
	gpg     *core.GPGInfo                 // nil until read
	keys    []core.OpenPGPKey             // the user's secret keys, as last read
	byAcc   map[int64]core.AccountOpenPGP // each account's settings, as last read or saved
	reading bool
	busy    bool // a save or a gpg choice runs
}

// read reads gpg, the keys and every account's settings anew.
func (v *openpgpView) read(a *App) {
	if v.reading {
		return
	}
	v.reading = true
	var info core.GPGInfo
	var keys []core.OpenPGPKey
	by := map[int64]core.AccountOpenPGP{}
	a.query(func(ctx context.Context) error {
		var err error
		if info, err = a.svc.OpenPGPStatus(ctx); err != nil {
			return err
		}
		if info.Error == "" {
			if keys, err = a.svc.OpenPGPKeys(ctx); err != nil {
				return err
			}
		}
		for _, acc := range a.accounts {
			if isArchive(acc) {
				continue
			}
			if by[acc.ID], err = a.svc.AccountOpenPGP(ctx, acc.ID); err != nil {
				return err
			}
		}
		return nil
	}, func(err error) {
		v.reading = false
		if err != nil {
			a.toastErr(err)
			return
		}
		v.gpg, v.keys, v.byAcc = &info, keys, by
	})
}

// save writes o as the account's settings.
func (v *openpgpView) save(a *App, id int64, o core.AccountOpenPGP) {
	if v.busy {
		return
	}
	v.busy = true
	var got core.AccountOpenPGP
	a.do(func(ctx context.Context) (err error) {
		got, err = a.svc.SetAccountOpenPGP(ctx, id, o)
		return err
	}, func(err error) {
		v.busy = false
		if err != nil {
			a.toastErr(err)
			return
		}
		v.byAcc[id] = got
	})
}

// chooseGPG asks for the gpg program in a file dialog; reset finds it again.
func (v *openpgpView) chooseGPG(a *App, reset bool) {
	if v.busy {
		return
	}
	v.busy = true
	plat := a.plat
	var info core.GPGInfo
	chosen := reset
	a.do(func(ctx context.Context) (err error) {
		path := ""
		if !reset {
			paths, err := plat.OpenFiles(false)
			if err != nil || len(paths) == 0 {
				return err
			}
			path, chosen = paths[0], true
		}
		info, err = a.svc.SetGPGPath(ctx, path)
		return err
	}, func(err error) {
		v.busy = false
		if err != nil {
			a.toastErr(err)
			return
		}
		if chosen {
			v.gpg = &info
			v.read(a) // its keys
		}
	})
}

// section draws the account's OpenPGP settings.
func (v *openpgpView) section(c *ui.Context, a *App, acc core.Account) {
	if v.byAcc == nil && !v.reading {
		v.byAcc = map[int64]core.AccountOpenPGP{}
		v.read(a)
	}
	gpgRow := func() {
		desc := "…"
		if v.gpg != nil {
			if v.gpg.Error != "" {
				desc = i18n.T("settings.account.openpgp.gpg.missing")
			} else {
				desc = v.gpg.Path + " · " + v.gpg.Version
			}
		}
		settingRow(c, "GnuPG", desc, func() ui.Element {
			ui.Row(c).Gap(c.Theme().Space(3)).AlignItems(ui.Center).Children(func() {
				rowButton(c, i18n.T("settings.account.openpgp.gpg.choose"), kit.Outline, v.busy).OnClick(func() { v.chooseGPG(a, false) })
				if v.gpg != nil && v.gpg.Configured != "" {
					rowButton(c, i18n.T("settings.account.openpgp.gpg.reset"), kit.Outline, v.busy).OnClick(func() { v.chooseGPG(a, true) })
				}
			})
			return ui.Element{}
		})
	}
	rows := []func(){gpgRow}
	if v.gpg != nil && v.gpg.Error == "" {
		cur := v.byAcc[acc.ID]
		opts := []kit.Option[string]{{Value: "", Label: i18n.T("settings.account.openpgp.key.none")}}
		for _, k := range v.keys {
			if k.CanSign || k.Fingerprint == cur.Key {
				opts = append(opts, kit.Option[string]{Value: k.Fingerprint, Label: keyLabel(k), Disabled: !k.CanSign})
			}
		}
		rows = append(rows, func() {
			settingRow(c, i18n.T("settings.account.openpgp.key"), i18n.T("settings.account.openpgp.key.help"), func() ui.Element {
				sel := cur.Key
				field := settingSelect(c, i18n.T("settings.account.openpgp.key"), &sel, opts)
				if sel != cur.Key {
					o := cur
					if o.Key = sel; sel == "" {
						o.SignByDefault, o.EncryptWhenPossible = false, false
					} else if cur.Key == "" {
						o.SignByDefault = true // choosing a key is choosing to sign
					}
					v.save(a, acc.ID, o)
				}
				return field
			})
		}, func() {
			on := cur.SignByDefault
			if kit.FieldWith(c, kit.FieldOpts{Horizontal: true, Switch: &on, Disabled: cur.Key == "" || v.busy}, i18n.T("settings.account.openpgp.sign"), i18n.T("settings.account.openpgp.sign.help"), "", nil).Changed() && cur.Key != "" {
				o := cur
				o.SignByDefault = on
				v.save(a, acc.ID, o)
			}
		}, func() {
			on := cur.EncryptWhenPossible
			if kit.FieldWith(c, kit.FieldOpts{Horizontal: true, Switch: &on, Disabled: cur.Key == "" || v.busy}, i18n.T("settings.account.openpgp.encrypt"), i18n.T("settings.account.openpgp.encrypt.help"), "", nil).Changed() && cur.Key != "" {
				o := cur
				o.EncryptWhenPossible = on
				v.save(a, acc.ID, o)
			}
		})
	}
	settingsSection(c, "OpenPGP", i18n.T("settings.account.openpgp.help"), rows...)
}

// keyLabel names a key in the picker: its first user ID and the last 16
// digits of its fingerprint, as gpg shows a long key ID.
func keyLabel(k core.OpenPGPKey) string {
	uid := ""
	if len(k.UserIDs) > 0 {
		uid = k.UserIDs[0] + " · "
	}
	id := k.Fingerprint
	if len(id) > 16 {
		id = id[len(id)-16:]
	}
	return uid + groupFingerprint(strings.ToUpper(id))
}
