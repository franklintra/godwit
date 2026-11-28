package core

import (
	"context"
	"testing"
	"time"

	"github.com/godwit-email/godwit/core/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// endlessRead runs until its context ends.
const endlessRead = "SELECT count(*) FROM (WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM c) SELECT x FROM c)"

// holdLane runs endlessRead on ctx's lane until the test ends.
func holdLane(t *testing.T, s *Service, ctx context.Context) {
	t.Helper()
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		_, err := s.db.Query(ctx, endlessRead)
		done <- err
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	time.Sleep(100 * time.Millisecond) // it has started
}

// The interface's calls run on their own readers (ADR 0052): while a
// background scan holds B, a window's calls still answer, and the write a
// click makes (SetFlags) is seen by the next GetThread.
func TestInteractiveReadsBesideBackground(t *testing.T) {
	e := newSyncEnv(t)
	e.put(t, "INBOX", msg("lanes@x"))
	e.sync(t)
	id := e.idOf(t, "lanes@x")
	ui := store.WithLane(WithUISession(t.Context(), e.s), store.Interactive, "SetFlags")

	holdLane(t, e.s, bg) // a background scan on B
	quick, stop := context.WithTimeout(ui, 3*time.Second)
	defer stop()
	require.NoError(t, e.s.SetFlags(quick, []int64{id}, []string{`\Flagged`}, nil))
	accs, err := e.s.ListAccounts(quick)
	require.NoError(t, err)
	assert.Len(t, accs, 1)
	page, err := e.s.ListThreads(quick, View{Unified: "inbox"}, "", 50)
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	th, err := e.s.GetThread(quick, page.Items[0].ID)
	require.NoError(t, err)
	require.Len(t, th.Messages, 1)
	assert.Contains(t, th.Messages[0].Flags, `\Flagged`, "the read sees the click's committed write")
	_, err = e.s.ThreadDeadline(quick, page.Items[0].ID)
	require.NoError(t, err)
	require.NoError(t, e.s.SetFlags(quick, []int64{id}, nil, []string{`\Flagged`}))
	th, err = e.s.GetThread(quick, page.Items[0].ID)
	require.NoError(t, err)
	assert.NotContains(t, th.Messages[0].Flags, `\Flagged`)

	// A badge read (UIBackground) waits for B, which the scan holds.
	short, stop2 := context.WithTimeout(store.WithLane(ui, store.UIBackground, "SavedViews"), 200*time.Millisecond)
	defer stop2()
	_, err = e.s.SavedViews(short)
	assert.ErrorIs(t, err, context.DeadlineExceeded, "UIBackground queues for B")
}

// A Wit run started from the window is the backend's work: its reads take
// B, not the interactive readers.
func TestRunCallerIsBackground(t *testing.T) {
	e := newSyncEnv(t)
	ui := store.WithLane(WithUISession(t.Context(), e.s), store.Interactive, "Ask")
	l, _ := store.LaneOf(runCaller(ui))
	assert.Equal(t, store.Background, l)
}
