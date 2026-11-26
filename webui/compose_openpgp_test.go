package webui

import (
	"testing"

	"github.com/godwit-email/godwit/core"
	"github.com/godwit-email/godwit/internal/gpg/gpgtest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The page offers OpenPGP once the account has a key, sets the draft's
// choices against its revision, and names recipients without a key.
func TestComposeOpenPGP(t *testing.T) {
	k := gpgtest.New(t)
	fpr := k.Gen("Alice <alice@example.com>")
	bob := gpgtest.New(t)
	bobFpr := bob.Gen("Bob <bob@example.com>")
	k.Import(bob.Export(bobFpr))
	k.Certify(bobFpr)
	t.Setenv("GNUPGHOME", k.GPG.Home)
	f := newFixture(t)
	d := f.draft(t, "new", 0)
	f.c.Attach(1, d.ID)
	st, err := f.page.Load(f.ctx(1))
	require.NoError(t, err)
	assert.Equal(t, OpenPGP{Available: false, Sign: false, Encrypt: core.OpenPGPEncryptOff}, st.OpenPGP)

	_, err = f.e.Svc.SetAccountOpenPGP(t.Context(), f.acc.ID, core.AccountOpenPGP{Key: fpr, SignByDefault: true})
	require.NoError(t, err)
	st, err = f.page.Load(f.ctx(1))
	require.NoError(t, err)
	assert.Equal(t, OpenPGP{Available: true, Sign: true, Encrypt: core.OpenPGPEncryptOff}, st.OpenPGP)

	ch, err := f.page.SetOpenPGP(f.ctx(1), true, core.OpenPGPEncryptOn, st.Revision)
	require.NoError(t, err)
	assert.False(t, ch.Conflict)
	assert.Equal(t, st.Revision+1, ch.Revision)
	assert.Equal(t, OpenPGP{Available: true, Sign: true, Encrypt: core.OpenPGPEncryptOn}, ch.OpenPGP)
	assert.Equal(t, core.DraftOpenPGP{Sign: true, Encrypt: core.OpenPGPEncryptOn}, f.stored(t, d.ID).OpenPGP)

	ch, err = f.page.SetOpenPGP(f.ctx(1), false, core.OpenPGPEncryptOff, st.Revision)
	require.NoError(t, err)
	assert.True(t, ch.Conflict, "a stale revision changes nothing")

	missing, err := f.page.OpenPGPMissing(f.ctx(1), []string{"Bob <bob@example.com>", "carol@example.com", "half-typed@"})
	require.NoError(t, err)
	assert.Equal(t, []string{"carol@example.com"}, missing)
}
