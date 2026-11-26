package core_test

import (
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/godwit-email/godwit/core"
	"github.com/godwit-email/godwit/core/coretest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Off, the plugin registry is not refreshed in the background; a refresh
// asked for still fetches it, and turning the switch on refreshes at once.
func TestRegistrySwitchOff(t *testing.T) {
	core.SetRegistryFirstForTest(t, 10*time.Millisecond)
	rs := coretest.NewRegistryServer(t)
	rs.Publish(t, coretest.Registry(t0))
	var hits atomic.Int32
	rs.Intercept(func(http.ResponseWriter, *http.Request, []byte) bool { hits.Add(1); return false })
	dir := t.TempDir()
	s := regService(t, nil, dir)
	st, err := s.GetSettings(ctx)
	require.NoError(t, err)
	st.PluginRegistry = false
	_, err = s.SetSettings(ctx, st)
	require.NoError(t, err)
	require.NoError(t, s.Close())

	s = regService(t, rs, dir)
	time.Sleep(200 * time.Millisecond) // many times registryFirst
	assert.Zero(t, hits.Load(), "no background refresh while off")

	require.NoError(t, s.RefreshPluginRegistry(ctx))
	assert.EqualValues(t, 2, hits.Load(), "the registry and its signature, asked for")

	st, err = s.GetSettings(ctx)
	require.NoError(t, err)
	st.PluginRegistry = true
	_, err = s.SetSettings(ctx, st)
	require.NoError(t, err)
	require.Eventually(t, func() bool { return hits.Load() >= 4 }, 5*time.Second, 10*time.Millisecond, "refreshed as it is turned on")
}
