package app

import (
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/godwit-email/godwit/core/coretest"
	"github.com/godwit-email/godwit/internal/i18n"
)

// 网络隐私 lists each connection Godwit makes by itself with its switch, all
// on by default; each switch saves its own setting, and the rows set
// elsewhere open their pages.
func TestSettingsNetworkPrivacy(t *testing.T) {
	h, _ := withAccount(t)
	h.resize(1440, 1600)
	h.openSettings("network")
	st := h.app.settings
	if !st.UpdateChecks || !st.PluginRegistry || !st.ModelCatalog || !st.Gravatar || !st.Favicons || !st.BIMI {
		t.Fatalf("defaults: %+v, want every connection on", st)
	}
	for _, sw := range []struct {
		key string
		on  func() bool
	}{
		{"settings.network.updates", func() bool { return h.app.settings.UpdateChecks }},
		{"settings.network.registry", func() bool { return h.app.settings.PluginRegistry }},
		{"settings.network.catalog", func() bool { return h.app.settings.ModelCatalog }},
		{"settings.network.gravatar", func() bool { return h.app.settings.Gravatar }},
		{"settings.network.favicons", func() bool { return h.app.settings.Favicons }},
		{"settings.network.bimi", func() bool { return h.app.settings.BIMI }},
	} {
		h.see(i18n.T(sw.key + ".help"))
		h.click(i18n.T(sw.key))
		h.until(sw.key+" off", func() bool { return !sw.on() })
	}
	saved, err := h.env.Svc.GetSettings(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if saved.UpdateChecks || saved.PluginRegistry || saved.ModelCatalog || saved.Gravatar || saved.Favicons || saved.BIMI {
		t.Fatalf("saved: %+v, want every connection off", saved)
	}
	h.click(i18n.T("settings.proxy.system"))
	h.until("General opens", func() bool { sec, _ := h.app.pages[PageSettings].(*settingsPage).section(); return sec == "general" })
}

// Automatic update checks off, nothing asks the update feed by itself: About
// offers Check for Updates, not the newer version, until they are on again.
func TestSettingsUpdateChecksOff(t *testing.T) {
	h, _ := withAccount(t)
	h.plat.mu.Lock()
	h.plat.latest = "9.0.0"
	h.plat.mu.Unlock()
	st := h.app.settings
	st.UpdateChecks = false
	h.app.changeSettings(st)
	h.openSettings("about")
	h.seeExactly(i18n.T("settings.about.updates.check"))
	if h.app.latest != "" {
		t.Fatalf("asked the update feed with checks off: %q", h.app.latest)
	}
	st.UpdateChecks = true
	h.app.changeSettings(st)
	h.until("the newer version shows", func() bool { return h.app.latest == "9.0.0" })
}

// Directory updates off, opening 插件 fetches nothing; the directory offers
// to update it once, which fetches it.
func TestSettingsPluginRegistryOff(t *testing.T) {
	rs := coretest.NewRegistryServer(t)
	rs.Publish(t, coretest.Registry(regT0))
	var hits atomic.Int32
	rs.Intercept(func(http.ResponseWriter, *http.Request, []byte) bool { hits.Add(1); return false })
	h := newHarnessWith(t, []coretest.Option{coretest.WithRegistry(rs.Src)}, func(e *coretest.Env) { e.AddAccount(t, "alice@example.com") })
	h.until("initial load", func() bool { return h.app.loaded })
	st := h.app.settings
	st.PluginRegistry = false
	h.app.changeSettings(st)
	h.openSettings("plugins")
	h.click(i18n.T("settings.ext.browse"))
	h.see(i18n.T("settings.ext.directory.paused"))
	if n := hits.Load(); n != 0 {
		t.Fatalf("opening 插件 asked the registry %d times with directory updates off", n)
	}
	h.click(i18n.T("settings.ext.directory.update"))
	h.until("fetched once asked", func() bool { return hits.Load() == 2 })
}
