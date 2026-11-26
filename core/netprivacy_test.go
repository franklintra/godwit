package core

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"
)

const testCatalog = `{"acme": {"api": "https://llm.acme.test/v1", "models": {"big": {"name": "Big One", "limit": {"context": 100000}}}}}`

// catalogServer serves testCatalog and counts the requests it answers.
func catalogServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		fmt.Fprint(w, testCatalog)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// saveSettings stores what edit changes in dir's settings, with no Service
// left running, so the next New starts with them.
func saveSettings(t *testing.T, dir string, edit func(*Settings)) {
	t.Helper()
	keyring.MockInit()
	s, err := New(t.Context(), Options{DataDir: dir})
	require.NoError(t, err)
	st, err := s.GetSettings(t.Context())
	require.NoError(t, err)
	edit(&st)
	_, err = s.SetSettings(t.Context(), st)
	require.NoError(t, err)
	require.NoError(t, s.Close())
}

func setSwitch(t *testing.T, s *Service, edit func(*Settings)) {
	t.Helper()
	st, err := s.GetSettings(t.Context())
	require.NoError(t, err)
	edit(&st)
	_, err = s.SetSettings(t.Context(), st)
	require.NoError(t, err)
}

func TestNetworkSwitchesDefaultOn(t *testing.T) {
	st := defaultSettings()
	assert.True(t, st.ModelCatalog && st.PluginRegistry && st.Gravatar && st.Favicons && st.BIMI && st.UpdateChecks, "today's behaviour by default: %+v", st)
}

// Off, models.dev is never asked: not at start, not for an unlisted model;
// turned on, the stale catalog is fetched at once.
func TestModelCatalogSwitchOff(t *testing.T) {
	srv, hits := catalogServer(t)
	dir := t.TempDir()
	saveSettings(t, dir, func(st *Settings) { st.ModelCatalog = false })
	s, err := New(t.Context(), Options{DataDir: dir, ModelsDev: srv.URL})
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })

	assert.Equal(t, defaultHistoryBudget, s.historyBudget("https://llm.acme.test/v1", "big"), "an unlisted model")
	assert.Empty(t, s.catalogNames(ModelService{BaseURL: "https://llm.acme.test/v1", Models: []string{"big"}}))
	s.fetchCatalog(t.Context()) // the refresh loop's own call
	time.Sleep(100 * time.Millisecond)
	assert.Zero(t, hits.Load(), "no request while the switch is off")
	assert.NoFileExists(t, filepath.Join(dir, catalogFile))

	setSwitch(t, s, func(st *Settings) { st.ModelCatalog = true })
	require.Eventually(t, func() bool { return s.historyBudget("https://llm.acme.test/v1", "big") == 80000 },
		5*time.Second, 10*time.Millisecond, "fetched as it is turned on")
	assert.EqualValues(t, 1, hits.Load())
}

// GODWIT_OFFLINE_DEFAULTS=1 turns every switch off by default; nothing else
// changes, and models.dev is not asked.
func TestOfflineDefaults(t *testing.T) {
	t.Setenv("GODWIT_OFFLINE_DEFAULTS", "1")
	srv, hits := catalogServer(t)
	s, err := New(t.Context(), Options{DataDir: t.TempDir(), ModelsDev: srv.URL})
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	st, err := s.GetSettings(t.Context())
	require.NoError(t, err)
	assert.False(t, st.ModelCatalog || st.PluginRegistry || st.Gravatar || st.Favicons || st.BIMI || st.UpdateChecks, "%+v", st)
	assert.Equal(t, "auto", st.RemoteImages)
	assert.Equal(t, ProxySystem, st.ProxyMode)
	time.Sleep(100 * time.Millisecond)
	assert.Zero(t, hits.Load())
}

// The catalog goes through the proxy set in Godwit, as mail and images do.
func TestModelCatalogThroughProxy(t *testing.T) {
	var asked atomic.Value
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Store(r.URL.String()) // an absolute URL: asked as a proxy
		fmt.Fprint(w, testCatalog)
	}))
	t.Cleanup(proxy.Close)
	dir := t.TempDir()
	saveSettings(t, dir, func(st *Settings) { st.ProxyMode, st.ProxyURL = ProxyCustom, proxy.URL })
	s, err := New(t.Context(), Options{DataDir: dir, ModelsDev: "http://models.dev.test/api.json"})
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	require.Eventually(t, func() bool { return s.historyBudget("https://llm.acme.test/v1", "big") == 80000 },
		5*time.Second, 10*time.Millisecond)
	assert.Equal(t, "http://models.dev.test/api.json", asked.Load())
}

// Each avatar source switched off is neither asked nor looked up in DNS;
// the others go on as before.
func TestSenderAvatarSourceSwitches(t *testing.T) {
	good := avatarPNG(t)
	f := newAvatarFixture(t, func(w http.ResponseWriter, r *http.Request) { w.Write(good) })
	var mu sync.Mutex
	var lookups []string
	f.s.opts.LookupTXT = func(_ context.Context, name string) ([]string, error) {
		mu.Lock()
		lookups = append(lookups, name)
		mu.Unlock()
		return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
	}
	looked := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), lookups...)
	}
	sources := func(gravatar, favicons, bimi bool) {
		setSwitch(t, f.s, func(st *Settings) { st.Gravatar, st.Favicons, st.BIMI = gravatar, favicons, bimi })
	}

	sources(false, false, false)
	got, err := f.s.SenderAvatar(t.Context(), addMessage(t, f.s, "a", "Bob <bob@example.com>", "plain", "", 1), false)
	require.NoError(t, err)
	assert.Empty(t, got.Data)
	assert.Empty(t, f.hits(), "no source asked")
	assert.Empty(t, looked(), "no DNS")

	sources(true, false, false)
	got, err = f.s.SenderAvatar(t.Context(), addMessage(t, f.s, "b", "Bob <bob@example.org>", "plain", "", 2), false)
	require.NoError(t, err)
	assert.Equal(t, "gravatar", got.Source)
	require.Len(t, f.hits(), 1)
	assert.True(t, strings.HasPrefix(f.hits()[0], "gravatar.com/avatar/"), f.hits())
	assert.Empty(t, looked())

	sources(false, true, false)
	got, err = f.s.SenderAvatar(t.Context(), addMessage(t, f.s, "c", "Bob <bob@example.net>", "plain", "", 3), false)
	require.NoError(t, err)
	assert.Equal(t, "favicon", got.Source)
	assert.Equal(t, "example.net/apple-touch-icon.png", f.hits()[len(f.hits())-1])
	assert.Empty(t, looked())
	n := len(f.hits())

	sources(false, false, true)
	got, err = f.s.SenderAvatar(t.Context(), addMessage(t, f.s, "d", "Bob <bob@example.edu>", "plain", "", 4), false)
	require.NoError(t, err)
	assert.Empty(t, got.Data, "no BIMI record")
	assert.Equal(t, []string{"default._bimi.example.edu"}, looked())
	assert.Len(t, f.hits(), n, "neither a favicon nor Gravatar")
}

// Site icons off, a model service shows no favicon and its site is not asked.
func TestModelServiceAvatarFaviconsOff(t *testing.T) {
	f := newAvatarFixture(t, func(w http.ResponseWriter, r *http.Request) { w.Write(avatarPNG(t)) })
	_, err := f.s.db.Execute(t.Context(), "INSERT INTO model_services(id, name, slug, kind, base_url, models) VALUES (7, 'OpenAI', 'oa', ?, 'https://api.openai.com/v1', '[]')", ProviderOpenAI)
	require.NoError(t, err)
	setSwitch(t, f.s, func(st *Settings) { st.Favicons = false })
	got, err := f.s.ModelServiceAvatar(t.Context(), 7)
	require.NoError(t, err)
	assert.Empty(t, got.Data)
	assert.Empty(t, f.hits())

	setSwitch(t, f.s, func(st *Settings) { st.Favicons = true })
	got, err = f.s.ModelServiceAvatar(t.Context(), 7)
	require.NoError(t, err)
	assert.Equal(t, "favicon", got.Source)
	assert.NotEmpty(t, f.hits(), "openai.com asked once it is on")
}

// BIMI off, the security scan asks DNS for no BIMI record and fetches no
// mark certificate; on, it looks the record up.
func TestSecurityScanBIMISwitch(t *testing.T) {
	ctx := t.Context()
	s := newLabelService(t, "", "de")
	var mu sync.Mutex
	var bimi []string
	s.securityLookupTXT = func(_ context.Context, name string) ([]string, error) {
		if strings.HasPrefix(name, "default._bimi.") {
			mu.Lock()
			bimi = append(bimi, name)
			mu.Unlock()
		}
		return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
	}
	looked := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), bimi...)
	}
	id := addMessage(t, s, "Rechnung", "Shop <billing@shop.example>", "Ihre Rechnung", "", time.Now().Unix())
	_, err := s.db.Execute(ctx, "UPDATE messages SET auth_results = 'mx.example.com; dmarc=pass header.from=shop.example; dkim=pass header.d=shop.example' WHERE id = ?", id)
	require.NoError(t, err)
	scan := func() string {
		t.Helper()
		_, err := s.RescanSecurity(ctx, 0, true)
		require.NoError(t, err)
		_, err = s.scanSecurityBatch(ctx)
		require.NoError(t, err)
		rows, err := s.db.Query(ctx, "SELECT security_status FROM messages WHERE id = ?", id)
		require.NoError(t, err)
		return rows.Values[0][0].(string)
	}

	setSwitch(t, s, func(st *Settings) { st.BIMI = false })
	assert.Equal(t, "verified_domain", scan())
	assert.Empty(t, looked())

	setSwitch(t, s, func(st *Settings) { st.BIMI = true })
	assert.Equal(t, "verified_domain", scan())
	assert.Equal(t, []string{"default._bimi.shop.example"}, looked())
}
