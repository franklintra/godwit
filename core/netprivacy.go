package core

// 网络隐私 (Network privacy): the switches of the connections Godwit makes by
// itself (Settings.ModelCatalog, PluginRegistry, Gravatar, Favicons, BIMI;
// UpdateChecks is the app's), kept in memory so a background loop or an
// avatar reads no database, and the client those connections take through
// the user's proxy (docs/adr/0046-network-privacy-switches.md).

import (
	"context"
	"net/http"
	"net/url"
	"time"
)

// netSwitches are the saved switches of 网络隐私 the backend acts on.
type netSwitches struct {
	catalog, registry, gravatar, favicons, bimi bool
}

func (s *Service) setNetSwitches(st Settings) {
	s.switches.Store(&netSwitches{catalog: st.ModelCatalog, registry: st.PluginRegistry,
		gravatar: st.Gravatar, favicons: st.Favicons, bimi: st.BIMI})
}

// netOn is the switches in effect: the saved ones (loadNetSwitches, at New).
func (s *Service) netOn() netSwitches {
	if p := s.switches.Load(); p != nil {
		return *p
	}
	d := defaultSettings()
	return netSwitches{catalog: d.ModelCatalog, registry: d.PluginRegistry, gravatar: d.Gravatar, favicons: d.Favicons, bimi: d.BIMI}
}

// loadNetSwitches reads the saved switches (at New).
func (s *Service) loadNetSwitches(ctx context.Context) error {
	st, err := s.storedSettings(ctx)
	if err != nil {
		return err
	}
	s.setNetSwitches(st)
	return nil
}

// netSwitched acts on switches st turned on since stored: what was skipped
// while they were off is fetched now, not at the next round (a day away for
// the registry). Turning one off needs nothing: each fetch reads netOn.
func (s *Service) netSwitched(stored, st Settings) {
	if st.ModelCatalog && !stored.ModelCatalog && s.opts.ModelsDev != "" && s.catalogAge() >= catalogStale {
		s.fetchCatalogInBackground()
	}
	if st.PluginRegistry && !stored.PluginRegistry && s.opts.Registry != nil {
		if ctx, end, err := s.begin(context.Background(), 0); err == nil {
			go func() {
				defer end()
				s.RefreshPluginRegistry(ctx) // a failure shows as RegistryView.Stale
			}()
		}
	}
}

// proxyClient is a client whose requests go through Proxy, as mail's and
// remote images' do, with the proxy resolving the host name, for the
// requests to fixed, well-known hosts: the model catalog, the plugin
// registry, the Store's catalog. timeout 0 leaves the limit to the
// request's context. A host a message names takes imagefetch's transport
// instead, which reaches public addresses only.
func (s *Service) proxyClient(timeout time.Duration) *http.Client {
	s.proxyOnce.Do(func() {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.Proxy = func(r *http.Request) (*url.URL, error) { return s.Proxy(r.URL), nil }
		s.proxyRT = t
	})
	return &http.Client{Transport: s.proxyRT, Timeout: timeout}
}
