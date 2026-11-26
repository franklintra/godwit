package core

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// OpenDraft and OpenMessage hand Options.ShowUI what to open once they know
// it exists; without ShowUI there is no window to show it in.
func TestOpenDraftAndMessage(t *testing.T) {
	var shown []string
	e := newSyncEnvWith(t, Options{ShowUI: func(what string) error {
		shown = append(shown, what)
		return nil
	}})
	e.put(t, "INBOX", eml("m@x", "bob@example.com", "alice@example.com", "Hi", "Mon, 05 Oct 2026 09:00:00 +0000", "hi"))
	e.sync(t)
	msg := e.rows(t, "SELECT id FROM messages")[0][0].(int64)
	d, err := e.s.NewDraft(bg, e.acc.ID, "reply", msg)
	require.NoError(t, err)

	require.NoError(t, e.s.OpenDraft(bg, d.ID))
	require.NoError(t, e.s.OpenMessage(bg, msg))
	assert.Equal(t, []string{"draft:" + strconv.FormatInt(d.ID, 10), "message:" + strconv.FormatInt(msg, 10)}, shown)
	id, ok := ParseOpenTarget(shown[0], OpenDraftPrefix)
	assert.True(t, ok)
	assert.Equal(t, d.ID, id)
	_, ok = ParseOpenTarget("draft:x", OpenDraftPrefix)
	assert.False(t, ok)
	_, ok = ParseOpenTarget(shown[1], OpenDraftPrefix)
	assert.False(t, ok)

	assert.Error(t, e.s.OpenDraft(bg, d.ID+100))
	assert.Error(t, e.s.OpenMessage(bg, msg+100))
	_, err = e.s.db.Execute(bg, "INSERT INTO outbox(account_id, state, draft_id) VALUES (?, 'queued', ?)", e.acc.ID, d.ID)
	require.NoError(t, err)
	assert.ErrorIs(t, e.s.OpenDraft(bg, d.ID), ErrStaleDraft, "a draft on its way out is not edited")
	assert.Len(t, shown, 2)

	e.s.opts.ShowUI = nil
	assert.ErrorIs(t, e.s.OpenMessage(bg, msg), ErrNoWindow)
}
