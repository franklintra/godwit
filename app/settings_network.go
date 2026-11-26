package app

import (
	"cmp"

	"github.com/egoist/mygo/ui"
	"github.com/godwit-email/godwit/core"
	"github.com/godwit-email/godwit/internal/i18n"
	"github.com/godwit-email/godwit/internal/icons"
	"github.com/godwit-email/godwit/internal/kit"
	"github.com/godwit-email/godwit/internal/theme"
)

// networkView is 网络隐私: every connection Godwit makes by itself, each row
// naming where it goes, with the switch that stops it; then remote images
// and the proxy, set on their own pages (docs/adr/0045-network-privacy-switches.md).
func (p *settingsPage) networkView(c *ui.Context) {
	sw := func(on *bool, title, help string) func() {
		return func() {
			kit.FieldWith(c, kit.FieldOpts{Horizontal: true, Switch: on}, i18n.T(title), i18n.T(help), "", nil)
		}
	}
	settingsSection(c, i18n.T("settings.network.godwit"), "",
		sw(&p.st.UpdateChecks, "settings.network.updates", "settings.network.updates.help"),
		sw(&p.st.PluginRegistry, "settings.network.registry", "settings.network.registry.help"),
		sw(&p.st.ModelCatalog, "settings.network.catalog", "settings.network.catalog.help"),
	)
	settingsSection(c, i18n.T("settings.network.avatars"), i18n.T("settings.network.avatars.foot"),
		sw(&p.st.Gravatar, "settings.network.gravatar", "settings.network.gravatar.help"),
		sw(&p.st.Favicons, "settings.network.favicons", "settings.network.favicons.help"),
		sw(&p.st.BIMI, "settings.network.bimi", "settings.network.bimi.help"),
	)
	t, th, a := theme.Of(c), c.Theme(), p.a
	// link is a row's value, a button that opens the page where it is set.
	link := func(label, value, page string) func() {
		return func() {
			settingRow(c, label, "", func() ui.Element {
				if kit.ButtonFrame(c, kit.ButtonOpts{Variant: kit.Ghost, Size: kit.SizeSm}).Label(value).Gap(th.Space(1)).TextColor(t.Muted).Children(func() {
					ui.Text(c, value).Role(ui.RoleNone) // the button's Label names it
					ui.Icon(c, icons.Get("chevron-right")).Size(theme.IconSm, theme.IconSm)
				}).Clicked() {
					a.navigate(Route{Page: PageSettings, Settings: page})
				}
				return ui.Element{}
			})
		}
	}
	images := map[string]string{"auto": "settings.remote_images.auto", "trusted": "settings.remote_images.trusted", "ask": "settings.remote_images.ask"}
	proxies := map[string]string{core.ProxySystem: "settings.proxy.system", core.ProxyCustom: "settings.proxy.custom", core.ProxyOff: "settings.proxy.off"}
	settingsSection(c, i18n.T("settings.network.elsewhere"), i18n.T("settings.network.foot"),
		link(i18n.T("settings.remote_images"), i18n.T(images[cmp.Or(p.st.RemoteImages, "auto")]), "images"),
		link(i18n.T("settings.proxy"), i18n.T(proxies[cmp.Or(p.st.ProxyMode, core.ProxySystem)]), "general"),
	)
}
