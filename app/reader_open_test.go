package app

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/godwit-email/godwit/core"
	"github.com/godwit-email/godwit/core/ai"
	"github.com/godwit-email/godwit/core/coretest"
	"github.com/godwit-email/godwit/core/events"
)

// openRecorder is the App's Service with the calls an open of a mail can
// make recorded by name, in order.
type openRecorder struct {
	Service
	mu    sync.Mutex
	calls []string
}

func (s *openRecorder) rec(m string) {
	s.mu.Lock()
	s.calls = append(s.calls, m)
	s.mu.Unlock()
}

// take returns the calls recorded since the last take.
func (s *openRecorder) take() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.calls
	s.calls = nil
	return out
}

func (s *openRecorder) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

func (s *openRecorder) OpenThread(ctx context.Context, threadID, messageID int64) (core.OpenedThread, error) {
	s.rec("OpenThread")
	return s.Service.OpenThread(ctx, threadID, messageID)
}

func (s *openRecorder) GetThread(ctx context.Context, id int64) (core.Thread, error) {
	s.rec("GetThread")
	return s.Service.GetThread(ctx, id)
}

func (s *openRecorder) ThreadLabels(ctx context.Context, ids []int64) (map[int64][]core.AppliedLabel, error) {
	s.rec("ThreadLabels")
	return s.Service.ThreadLabels(ctx, ids)
}

func (s *openRecorder) GetMessageLazyImages(ctx context.Context, id int64, remote bool) (core.Message, error) {
	s.rec("GetMessageLazyImages")
	return s.Service.GetMessageLazyImages(ctx, id, remote)
}

func (s *openRecorder) ImagesAllowed(ctx context.Context, id int64) (bool, error) {
	s.rec("ImagesAllowed")
	return s.Service.ImagesAllowed(ctx, id)
}

func (s *openRecorder) SetFlags(ctx context.Context, ids []int64, add, remove []string) error {
	s.rec("SetFlags")
	return s.Service.SetFlags(ctx, ids, add, remove)
}

func (s *openRecorder) ThreadDeadline(ctx context.Context, id int64) (core.ReaderDeadline, error) {
	s.rec("ThreadDeadline")
	return s.Service.ThreadDeadline(ctx, id)
}

func (s *openRecorder) ViewWhy(ctx context.Context, id, threadID int64) (core.ViewReason, error) {
	s.rec("ViewWhy")
	return s.Service.ViewWhy(ctx, id, threadID)
}

func (s *openRecorder) UnsubscribeMethod(ctx context.Context, id int64) (core.Unsubscribe, error) {
	s.rec("UnsubscribeMethod")
	return s.Service.UnsubscribeMethod(ctx, id)
}

func (s *openRecorder) PluginCards(ctx context.Context, id int64) ([]core.Card, error) {
	s.rec("PluginCards")
	return s.Service.PluginCards(ctx, id)
}

func (s *openRecorder) PluginTags(ctx context.Context, id int64) ([]core.Tag, error) {
	s.rec("PluginTags")
	return s.Service.PluginTags(ctx, id)
}

func (s *openRecorder) ListSkills(ctx context.Context) ([]core.SkillInfo, error) {
	s.rec("ListSkills")
	return s.Service.ListSkills(ctx)
}

func (s *openRecorder) ListActions(ctx context.Context, kind string) ([]core.ActionInfo, error) {
	s.rec("ListActions")
	return s.Service.ListActions(ctx, kind)
}

func (s *openRecorder) CachedAttachments(ctx context.Context, ids []int64) (map[int64]string, error) {
	s.rec("CachedAttachments")
	return s.Service.CachedAttachments(ctx, ids)
}

func (s *openRecorder) SenderAvatar(ctx context.Context, id int64, remote bool) (core.Avatar, error) {
	s.rec("SenderAvatar")
	return s.Service.SenderAvatar(ctx, id, remote)
}

func (s *openRecorder) ListAddressBooks(ctx context.Context) ([]core.AddressBook, error) {
	s.rec("ListAddressBooks")
	return s.Service.ListAddressBooks(ctx)
}

func (s *openRecorder) OpenPGPMessage(ctx context.Context, id int64) (core.OpenPGPView, error) {
	s.rec("OpenPGPMessage")
	return s.Service.OpenPGPMessage(ctx, id)
}

func (s *openRecorder) QuickReplies(ctx context.Context, id int64) ([]core.QuickReply, error) {
	s.rec("QuickReplies")
	return s.Service.QuickReplies(ctx, id)
}

func (s *openRecorder) MessageGist(ctx context.Context, id int64) (string, error) {
	s.rec("MessageGist")
	return s.Service.MessageGist(ctx, id)
}

func (s *openRecorder) RunSkill(ctx context.Context, name string, t core.Target) (<-chan ai.Event, error) {
	s.rec("RunSkill")
	return s.Service.RunSkill(ctx, name, t)
}

func (s *openRecorder) ContactAvatar(ctx context.Context, addr string) (core.Avatar, error) {
	s.rec("ContactAvatar")
	return s.Service.ContactAvatar(ctx, addr)
}

func (s *openRecorder) ContactAvatars(ctx context.Context, addrs []string) ([]core.Avatar, error) {
	s.rec("ContactAvatars")
	return s.Service.ContactAvatars(ctx, addrs)
}

// recordOpens puts an openRecorder in front of the App's Service and the
// reader's reads.
func recordOpens(h *harness) *openRecorder {
	rec := &openRecorder{Service: h.app.svc}
	h.app.svc = rec
	r := h.app.mail.reader
	r.getThread, r.openThread, r.getMessage, r.getAvatar, r.runSkill = rec.GetThread, rec.OpenThread, rec.GetMessageLazyImages, rec.SenderAvatar, rec.RunSkill
	r.cardsFn, r.tagsFn, r.quickFn, r.gistFn, r.skillsFn, r.actionsFn = rec.PluginCards, rec.PluginTags, rec.QuickReplies, rec.MessageGist, rec.ListSkills, rec.ListActions
	return rec
}

// settleOpen lets the refreshes an open caused run out.
func (h *harness) settleOpen() {
	h.t.Helper()
	for range 8 {
		h.advance(refreshEvery)
		time.Sleep(5 * time.Millisecond)
	}
}

// Opening a mail makes one call before its body paints (OpenThread); what
// shows beside the body (its read flag, reply date, cards, tags, skills,
// avatar, unsubscribe) is read once it has painted, and ImagesAllowed is
// asked only within the open.
func TestReaderOpenOneCallBeforePaint(t *testing.T) {
	h, accs := mailHarness(t, nil, "alice@example.com")
	a := h.deliverOne(accs[0], coretest.Msg("bob@example.com", accs[0].Email, "First thread", "first body"))
	b := h.deliverOne(accs[0], coretest.Msg("carol@example.com", accs[0].Email, "Second thread", "second body"))
	r := h.app.mail.reader
	h.openThread(a.ThreadID)
	h.seeBody(a.ID)
	h.settleOpen()

	rec := recordOpens(h)
	switchTo(h, b.ThreadID)
	vb := unseenBody(h, b.ID)
	for range 10 {
		h.drain()
		h.tt.Frame()
		time.Sleep(2 * time.Millisecond)
	}
	if got := rec.take(); !slices.Equal(got, []string{"OpenThread"}) {
		t.Fatalf("calls before the body painted: %v, want [OpenThread]", got)
	}
	h.app.BodyHeight(vb, b.ID, 120)
	h.app.BodyPainted(vb, b.ID)
	want := []string{"SetFlags", "ThreadDeadline", "PluginCards", "PluginTags", "ListSkills", "ListActions", "UnsubscribeMethod"}
	h.until("the calls beside the body", func() bool {
		got := rec.seen()
		return !slices.ContainsFunc(want, func(m string) bool { return !slices.Contains(got, m) }) &&
			(slices.Contains(got, "SenderAvatar") || slices.Contains(got, "ListAddressBooks"))
	})
	h.settleOpen()
	for _, m := range rec.seen() {
		switch m {
		case "ImagesAllowed", "GetMessageLazyImages": // OpenThread may come after: the next rows' prefetch
			t.Fatalf("%s after the open: %v", m, rec.seen())
		}
	}
	if r.key[0] != b.ThreadID || !slices.Contains(r.msgs[0].Flags, `\Seen`) {
		t.Fatalf("reader on %v, flags %v", r.key, r.msgs[0].Flags)
	}
}

// A page that never paints (a background window) does not hold what shows
// beside it for ever: it is read after sideCallsWait.
func TestReaderOpenSideCallsWithoutPaint(t *testing.T) {
	h, accs := mailHarness(t, nil, "alice@example.com")
	a := h.deliverOne(accs[0], coretest.Msg("bob@example.com", accs[0].Email, "First thread", "first body"))
	rec := recordOpens(h)
	h.plat.paintBody = nil
	h.app.mail.open([]mailItem{{ID: a.ThreadID, Thread: true}}, 0)
	h.until("the body read", func() bool { b := h.app.mail.reader.bodies[a.ID]; return b != nil && b.ready })
	h.drain()
	if slices.Contains(rec.seen(), "SetFlags") {
		t.Fatalf("read flag written before the paint: %v", rec.seen())
	}
	h.advance(sideCallsWait)
	h.until("the read flag written", func() bool { return slices.Contains(rec.seen(), "SetFlags") })
}

// The next row's open is read while the reader is idle: moving to it shows
// its body with no call before the paint. An event naming its thread drops
// it, as does any event naming nothing.
func TestReaderPrefetchedNextPaintsWithoutCall(t *testing.T) {
	h, accs := mailHarness(t, nil, "alice@example.com")
	for _, s := range []string{"One", "Two", "Three"} {
		h.deliverOne(accs[0], coretest.Msg("bob@example.com", accs[0].Email, s, s+" body"))
	}
	p, r := h.app.mail, h.app.mail.reader
	h.waitMail("three rows", func() bool { return len(p.shown()) == 3 })
	rows := p.shown()
	p.open(rows, 0)
	h.until("first open painted", func() bool { b := r.bodies[r.newestID()]; return b != nil && b.shown })
	h.settleOpen()
	h.post(r.prefetch) // again, should a refresh have dropped what the paint prefetched
	next := rowRoute(rows[1])
	h.until("the next row prefetched", func() bool { pf := r.pre[next]; return pf != nil && pf.ot != nil })
	if len(r.pre) != 1 { // the first row has no row before it
		t.Fatalf("prefetched %v", r.pre)
	}

	rec := recordOpens(h)
	h.plat.paintBody = nil
	p.open(rows, 1)
	h.until("the next body made", func() bool { return r.key == next && r.loaded && h.plat.view(r.newestID()) != nil })
	if got := rec.take(); len(got) != 0 {
		t.Fatalf("calls before a prefetched body painted: %v", got)
	}
	v := h.plat.view(r.newestID())
	h.app.BodyHeight(v, r.newestID(), 80)
	h.app.BodyPainted(v, r.newestID())
	h.until("the calls beside it", func() bool { return slices.Contains(rec.seen(), "ThreadDeadline") })

	// Its neighbours are read in turn; an event on one drops it.
	h.until("both neighbours prefetched", func() bool {
		return len(r.pre) == 2 && r.pre[rowRoute(rows[0])] != nil && r.pre[rowRoute(rows[2])] != nil &&
			r.pre[rowRoute(rows[0])].ot != nil && r.pre[rowRoute(rows[2])].ot != nil
	})
	r.prefetchEvent(events.Event{Type: events.MessagesChanged, ThreadIDs: []int64{rowRoute(rows[2])[0]}})
	if r.pre[rowRoute(rows[2])] != nil || r.pre[rowRoute(rows[0])] == nil {
		t.Fatalf("an event on one thread left %v", r.pre)
	}
	r.prefetchEvent(events.Event{Type: events.SyncStarted})
	if len(r.pre) != 1 {
		t.Fatal("a sync's start dropped a prefetched open")
	}
	r.prefetchEvent(events.Event{Type: events.MessagesChanged})
	if len(r.pre) != 0 {
		t.Fatalf("an event naming nothing left %v", r.pre)
	}
}

// newestID is the newest message the reader shows.
func (r *reader) newestID() int64 {
	if m, ok := r.last(); ok {
		return m.ID
	}
	return 0
}
