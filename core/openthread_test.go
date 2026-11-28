package core

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// OpenThread equals the four reads the reader made before it: GetThread,
// ThreadLabels, GetMessageLazyImages and ImagesAllowed, for every thread of
// the sample, opened as a thread and at each of its messages.
func TestOpenThreadEqualsItsReads(t *testing.T) {
	e := newSyncEnv(t)
	day := "Mon, 05 Oct 2026 %02d:00:00 +0000"
	e.put(t, "INBOX", eml("o@x", "Bob <bob@example.com>", "alice@example.com", "plan", fmt.Sprintf(day, 9), "question"))
	e.put(t, "Sent", eml("r@x", "alice@example.com", "bob@example.com", "Re: plan", fmt.Sprintf(day, 10), "answer", "In-Reply-To: <o@x>"))
	e.put(t, "INBOX", eml("r2@x", "Bob <bob@example.com>", "alice@example.com", "Re: plan", fmt.Sprintf(day, 11), "thanks", "In-Reply-To: <r@x>"))
	e.put(t, "INBOX", eml("solo@x", "Carol <carol@example.com>", "alice@example.com", "alone", fmt.Sprintf(day, 12), "one message"))
	e.put(t, "INBOX", "Message-ID: <html@x>\r\nFrom: Dan <dan@example.com>\r\nTo: alice@example.com\r\nSubject: news\r\n"+
		"Date: Mon, 05 Oct 2026 13:00:00 +0000\r\nMIME-Version: 1.0\r\nContent-Type: text/html; charset=utf-8\r\n\r\n"+
		"<p>Hello <img src=\"https://example.com/a.png\"></p>\r\n")
	e.put(t, "INBOX", withAttachments("att@x", "files here", 2))
	e.sync(t)
	require.NoError(t, e.s.TrustImageSender(bg, "dan@example.com"))
	l, err := e.s.CreateLabel(bg, LabelInput{Name: "Work", Color: "gray", Icon: "folder-3"})
	require.NoError(t, err)
	require.NoError(t, e.s.SetLabel(bg, []int64{e.idOf(t, "r2@x")}, l.ID, true))

	threads := map[int64]bool{}
	for _, mid := range []string{"o@x", "solo@x", "html@x", "att@x"} {
		threads[e.threadOf(t, mid)] = true
	}
	opened := 0
	for tid := range threads {
		th, err := e.s.GetThread(bg, tid)
		require.NoError(t, err)
		routes := []int64{0}
		for _, m := range th.Messages {
			routes = append(routes, m.ID)
		}
		for _, mid := range routes {
			got, err := e.s.OpenThread(bg, tid, mid)
			require.NoError(t, err)

			labels, err := e.s.ThreadLabels(bg, []int64{tid})
			require.NoError(t, err)
			want := OpenedThread{Thread: th, Labels: labels[tid], MessageID: mid}
			if mid == 0 {
				want.MessageID = th.Messages[len(th.Messages)-1].ID
			}
			want.Message, err = e.s.GetMessageLazyImages(bg, want.MessageID, false)
			require.NoError(t, err)
			want.ImagesAllowed, err = e.s.ImagesAllowed(bg, want.MessageID)
			require.NoError(t, err)
			require.Equal(t, want, got, "thread %d at message %d", tid, mid)
			opened++
		}
	}
	require.Equal(t, 4+6, opened) // four threads, six messages
	// The trusted sender's images load; the others ask.
	html, err := e.s.OpenThread(bg, e.threadOf(t, "html@x"), 0)
	require.NoError(t, err)
	require.True(t, html.ImagesAllowed)
	work, err := e.s.OpenThread(bg, e.threadOf(t, "o@x"), 0)
	require.NoError(t, err)
	require.Equal(t, []AppliedLabel{{LabelID: l.ID}}, work.Labels)
}

// A route to a message outside the thread opens no message, as the reader
// shows none then; a thread that is gone fails as GetThread does.
func TestOpenThreadRouteOutside(t *testing.T) {
	e := newSyncEnv(t)
	e.put(t, "INBOX", eml("a@x", "Bob <bob@example.com>", "alice@example.com", "one", "Mon, 05 Oct 2026 09:00:00 +0000", "a"))
	e.put(t, "INBOX", eml("b@x", "Carol <carol@example.com>", "alice@example.com", "two", "Mon, 05 Oct 2026 10:00:00 +0000", "b"))
	e.sync(t)
	got, err := e.s.OpenThread(bg, e.threadOf(t, "a@x"), e.idOf(t, "b@x"))
	require.NoError(t, err)
	require.Zero(t, got.MessageID)
	require.Zero(t, got.Message.ID)
	require.Len(t, got.Thread.Messages, 1)

	_, wantErr := e.s.GetThread(bg, 999999)
	_, err = e.s.OpenThread(bg, 999999, 0)
	require.EqualError(t, err, wantErr.Error())
}
