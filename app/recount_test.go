package app

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/godwit-email/godwit/core"
	"github.com/godwit-email/godwit/core/events"
)

// reads is how many times each of the refresh's reads ran so far.
type reads struct{ list, saved, accounts, starred int32 }

func (s *bootSvc) reads() reads {
	return reads{s.list.calls.Load(), s.saved.calls.Load(), s.accounts.calls.Load(), s.starred.calls.Load()}
}

func (r reads) since(old reads) reads {
	return reads{r.list - old.list, r.saved - old.saved, r.accounts - old.accounts, r.starred - old.starred}
}

// idle reports whether no read of the App's or the mail page's is in
// flight or due.
func idle(h *harness) bool {
	a, p := h.app, h.app.mail
	return a.MailReady() && p.viewsKnown && !p.viewsPending() && !p.list.loading && !p.listAgain && !p.waitLoad &&
		!a.loads.loading && !a.loadAgain && a.refreshStop == nil
}

// mailEvent refreshes for a sync's event of acc and waits until the reads
// it asked for have landed.
func mailEvent(h *harness, acc int64) {
	h.t.Helper()
	h.app.onEvent(events.Event{Type: events.MessagesChanged, AccountID: acc})
	h.advance(refreshEvery)
	h.until("the refresh read", func() bool { return idle(h) })
}

// countsHarness is a bootHarness settled after a first mail event, whose
// refresh read the counts: the next ones within countsEvery wait.
func countsHarness(t *testing.T) (*harness, *bootSvc, core.Account) {
	h, svc, accs := bootHarness(t, nil, "alice@example.com")
	h.until("settled", func() bool { return idle(h) })
	before := svc.reads()
	mailEvent(h, accs[0].ID)
	if got := svc.reads().since(before); got.accounts != 1 || got.saved != 1 || got.starred != 1 || got.list != 1 {
		t.Fatalf("the first mail event's refresh read %+v, want everything once", got)
	}
	return h, svc, accs[0]
}

// While a sync's events keep coming, each refresh reads the list again
// (it shows the change) but leaves the counts as they were; countsEvery
// after they were read, one refresh reads them, and shows the change.
func TestCountsWaitDuringSyncBurst(t *testing.T) {
	h, svc, acc := countsHarness(t)
	if h.app.unread[acc.ID] != 1 {
		t.Fatalf("unread %v", h.app.unread)
	}
	ids := inboxIDs(t, h, acc)
	before := svc.reads()

	// Read elsewhere (a sync, another client): the backend's event reaches the App.
	n := h.app.events
	if err := h.env.Svc.SetFlags(context.Background(), ids, []string{`\Seen`}, nil); err != nil {
		t.Fatal(err)
	}
	h.until("the event", func() bool { return h.app.events > n })
	h.advance(refreshEvery)
	h.until("the refresh read", func() bool { return idle(h) })
	for range 10 {
		h.app.onEvent(events.Event{Type: events.MessageReceived, AccountID: acc.ID})
		h.advance(time.Second)
		h.until("the refresh read", func() bool { return idle(h) })
	}
	got := svc.reads().since(before)
	if got.accounts != 0 || got.saved != 0 || got.starred != 0 {
		t.Fatalf("the burst read the counts: %+v", got)
	}
	if got.list < 11 {
		t.Fatalf("%d list reads for 11 refreshes", got.list)
	}
	if !h.app.MailReady() || len(h.app.mail.items) != 1 || h.app.mail.items[0].Unread != 0 {
		t.Fatalf("ready %v, the list shows %+v", h.app.MailReady(), h.app.mail.items)
	}
	if h.app.unread[acc.ID] != 1 || !h.app.countsOwed || h.app.countsTimer == nil {
		t.Fatalf("unread %v, owed %v", h.app.unread, h.app.countsOwed)
	}

	before = svc.reads()
	h.advance(countsEvery) // the burst is over: the trailing refresh
	h.until("the trailing refresh read", func() bool { return idle(h) })
	got = svc.reads().since(before)
	if got.accounts != 1 || got.saved != 1 || got.starred != 1 {
		t.Fatalf("the trailing refresh read %+v, want the counts once", got)
	}
	if h.app.unread[acc.ID] != 0 || h.app.countsOwed || h.app.countsTimer != nil || !h.app.MailReady() {
		t.Fatalf("unread %v, owed %v, ready %v", h.app.unread, h.app.countsOwed, h.app.MailReady())
	}

	before = svc.reads()
	h.advance(2 * countsEvery) // nothing owed: no other refresh
	h.drain()
	if got := svc.reads().since(before); got != (reads{}) {
		t.Fatalf("read %+v with nothing owed", got)
	}
}

// A burst that lasts reads the counts once per countsEvery.
func TestCountsEveryDuringLongBurst(t *testing.T) {
	h, svc, acc := countsHarness(t)
	before := svc.reads()
	for range 60 { // a minute of events, one a second
		h.app.onEvent(events.Event{Type: events.MessagesChanged, AccountID: acc.ID})
		h.advance(time.Second)
		h.until("the refresh read", func() bool { return idle(h) })
	}
	got := svc.reads().since(before)
	if got.saved < 2 || got.saved > 3 || got.accounts != got.saved || got.starred != got.saved {
		t.Fatalf("a minute of events read %+v, want the counts every %v", got, countsEvery)
	}
	if got.list < 60 {
		t.Fatalf("%d list reads for 60 refreshes", got.list)
	}
}

// Mail written here (a mark read) reads the counts at once, in a burst too.
func TestCountsAtOnceAfterMailWrite(t *testing.T) {
	h, svc, acc := countsHarness(t)
	ids := inboxIDs(t, h, acc)
	mailEvent(h, acc.ID) // a sync's: the counts wait
	if !h.app.countsOwed {
		t.Fatal("the counts did not wait")
	}
	before, start := svc.reads(), h.app.clock.Now()
	landed := false
	h.app.writes.Chain(fmt.Sprintf("account:%d", acc.ID), func(ctx context.Context) error {
		return h.app.svc.SetFlags(ctx, ids, []string{`\Seen`}, nil)
	}, func(error) { landed = true })
	h.until("the write", func() bool { return landed })
	h.waitMail("the refresh read", func() bool { return idle(h) && svc.reads().since(before).saved > 0 })
	if h.app.countsAt.Sub(start) > time.Second {
		t.Fatalf("the counts were read %v after the write", h.app.countsAt.Sub(start))
	}
	got := svc.reads().since(before)
	if got.accounts < 1 || got.starred < 1 || got.saved < 1 {
		t.Fatalf("the write's refresh read %+v, want the counts", got)
	}
	if h.app.unread[acc.ID] != 0 || h.app.countsOwed {
		t.Fatalf("unread %v, owed %v", h.app.unread, h.app.countsOwed)
	}
}

// An event of an account the App does not know yet (added elsewhere)
// reads the counts, and so the accounts, at once.
func TestCountsAtOnceForNewAccount(t *testing.T) {
	h, svc, acc := countsHarness(t)
	before := svc.reads()
	mailEvent(h, acc.ID+100)
	if got := svc.reads().since(before); got.accounts != 1 || got.saved != 1 {
		t.Fatalf("an unknown account's event read %+v, want the counts", got)
	}
}

// A switch to another view reads its list alone when nothing is owed; to a
// saved view, it reads the views at once; with counts owed, it reads them.
func TestViewSwitchCounts(t *testing.T) {
	h, svc, acc := countsHarness(t)
	before := svc.reads()
	h.app.navigate(Route{Page: PageMail, View: core.View{FolderID: h.folderOf(acc, "inbox").ID}})
	h.until("the folder's list", func() bool { return idle(h) })
	if got := svc.reads().since(before); got != (reads{list: 1}) {
		t.Fatalf("a folder switch read %+v, want the list alone", got)
	}
	if !h.tt.HasText("hello 0") {
		t.Fatalf("the folder misses its row; texts %q", h.tt.Texts())
	}

	id, err := h.env.Svc.SaveView(context.Background(), core.SavedView{Name: "Bob", Icon: "team", Doc: core.SavedViewDoc{Match: core.MatchAll,
		Conditions: []core.RuleCondition{{Field: core.FieldFrom, Op: core.OpContains, Value: "bob"}}}})
	if err != nil {
		t.Fatal(err)
	}
	h.waitMail("the view", func() bool {
		return idle(h) && slices.ContainsFunc(h.app.mail.views, func(b core.SavedView) bool { return b.ID == id })
	})
	before = svc.reads()
	h.app.navigate(Route{Page: PageMail, View: savedRoute(id)})
	h.until("the view's rows", func() bool { return idle(h) })
	if got := svc.reads().since(before); got.saved != 1 {
		t.Fatalf("a saved view's switch read %+v, want the views at once", got)
	}
	if len(h.app.mail.items) != 1 {
		t.Fatalf("the view lists %+v", h.app.mail.items)
	}

	h.app.navigate(Route{Page: PageMail, View: core.View{Unified: "inbox"}})
	h.until("the inbox", func() bool { return idle(h) })
	mailEvent(h, acc.ID) // within countsEvery of the last read: owed
	if !h.app.countsOwed {
		t.Fatal("the counts did not wait")
	}
	before = svc.reads()
	h.app.navigate(Route{Page: PageMail, View: core.View{Unified: "starred"}})
	h.until("starred", func() bool { return idle(h) })
	if got := svc.reads().since(before); got.starred != 1 || got.saved != 1 {
		t.Fatalf("a switch with counts owed read %+v, want them", got)
	}
}

// A switch while a refresh reads the counts drops that read; the switch
// reads them in its place, or the sidebar keeps the counts from before the
// change with nothing owed to correct them.
func TestViewSwitchKeepsCountsInFlight(t *testing.T) {
	h, svc, acc := countsHarness(t)
	ids := inboxIDs(t, h, acc)
	h.advance(countsEvery) // the next event's refresh reads the counts
	svc.listGate.close()
	n := h.app.events
	if err := h.env.Svc.SetFlags(context.Background(), ids, []string{`\Seen`}, nil); err != nil {
		t.Fatal(err)
	}
	h.until("the event", func() bool { return h.app.events > n })
	h.advance(refreshEvery)
	h.until("the refresh held at its list", func() bool { return svc.list.in.Load() == 1 && h.app.mail.listCounts })
	if h.app.countsOwed {
		t.Fatal("the refresh did not read the counts")
	}

	h.app.navigate(Route{Page: PageMail, View: core.View{Unified: "starred"}})
	h.until("the held read cancelled", func() bool { return svc.listGate.cancelled.Load() == 1 })
	svc.listGate.open()
	h.until("starred", func() bool { return idle(h) })
	h.advance(3 * countsEvery)
	h.drain()
	h.until("idle", func() bool { return idle(h) })

	inbox := h.folderOf(acc, "inbox").ID
	for _, f := range h.app.mail.folders[acc.ID] {
		if f.ID == inbox && f.Unread != 0 {
			t.Fatalf("the sidebar still shows %d unread in the inbox (unread %v)", f.Unread, h.app.unread)
		}
	}
	if h.app.unread[acc.ID] != 0 {
		t.Fatalf("unread %v", h.app.unread)
	}
}

// A switch away from a saved view while its run reads the views drops
// that read; the views are read again once the new list lands.
func TestViewSwitchKeepsViewsInFlight(t *testing.T) {
	h, svc, acc := countsHarness(t)
	id, err := h.env.Svc.SaveView(context.Background(), core.SavedView{Name: "Bob", Icon: "team", Doc: core.SavedViewDoc{Match: core.MatchAll,
		Conditions: []core.RuleCondition{{Field: core.FieldFrom, Op: core.OpContains, Value: "bob"}}}})
	if err != nil {
		t.Fatal(err)
	}
	h.waitMail("the view", func() bool {
		return idle(h) && slices.ContainsFunc(h.app.mail.views, func(b core.SavedView) bool { return b.ID == id })
	})
	h.app.navigate(Route{Page: PageMail, View: savedRoute(id)})
	h.until("the view's rows", func() bool { return idle(h) && len(h.app.mail.items) == 1 })
	unread := func() int {
		i := slices.IndexFunc(h.app.mail.views, func(b core.SavedView) bool { return b.ID == id })
		if i < 0 {
			t.Fatal("the view is gone")
		}
		return h.app.mail.views[i].Unread
	}
	if unread() != 1 {
		t.Fatalf("the view counts %d unread", unread())
	}

	h.advance(countsEvery) // the next event's refresh reads the views in its list run
	svc.savedGate.close()
	n := h.app.events
	if err := h.env.Svc.SetFlags(context.Background(), inboxIDs(t, h, acc), []string{`\Seen`}, nil); err != nil {
		t.Fatal(err)
	}
	h.until("the event", func() bool { return h.app.events > n })
	h.advance(refreshEvery)
	h.until("the views' read held", func() bool { return svc.saved.in.Load() == 1 && h.app.mail.listViews })

	h.app.navigate(Route{Page: PageMail, View: core.View{Unified: "inbox"}})
	h.until("the held read cancelled", func() bool { return svc.savedGate.cancelled.Load() == 1 })
	svc.savedGate.open()
	h.until("the inbox", func() bool { return idle(h) })
	h.advance(3 * countsEvery)
	h.drain()
	h.until("idle", func() bool { return idle(h) })
	if unread() != 0 {
		t.Fatalf("the sidebar still counts %d unread in the view", unread())
	}
}

// In a saved view, a sync's event reads the view's rows again, without
// the views' counts.
func TestSavedViewRowsDuringBurst(t *testing.T) {
	h, svc, acc := countsHarness(t)
	id, err := h.env.Svc.SaveView(context.Background(), core.SavedView{Name: "Bob", Icon: "team", Doc: core.SavedViewDoc{Match: core.MatchAll,
		Conditions: []core.RuleCondition{{Field: core.FieldFrom, Op: core.OpContains, Value: "bob"}}}})
	if err != nil {
		t.Fatal(err)
	}
	h.waitMail("the view", func() bool {
		return idle(h) && slices.ContainsFunc(h.app.mail.views, func(b core.SavedView) bool { return b.ID == id })
	})
	h.app.navigate(Route{Page: PageMail, View: savedRoute(id)})
	h.until("the view's rows", func() bool { return idle(h) && len(h.app.mail.items) == 1 })
	if h.app.mail.items[0].Unread != 1 {
		t.Fatalf("the view lists %+v", h.app.mail.items)
	}
	mailEvent(h, acc.ID) // the counts were read at the switch: the next ones wait
	if !h.app.countsOwed {
		t.Fatal("the counts did not wait")
	}

	before, n := svc.reads(), h.app.events
	if err := h.env.Svc.SetFlags(context.Background(), inboxIDs(t, h, acc), []string{`\Seen`}, nil); err != nil {
		t.Fatal(err)
	}
	h.until("the event", func() bool { return h.app.events > n })
	h.advance(refreshEvery)
	h.until("the view's rows", func() bool { return idle(h) && h.app.mail.items[0].Unread == 0 })
	if got := svc.reads().since(before); got.saved != 0 || got.starred != 0 || got.accounts != 0 {
		t.Fatalf("the view's rows read the counts: %+v", got)
	}
	if h.app.route.View != savedRoute(id) || !h.app.MailReady() {
		t.Fatalf("route %+v, ready %v", h.app.route, h.app.MailReady())
	}
}

// inboxIDs is the IDs of the messages in acc's inbox.
func inboxIDs(t *testing.T, h *harness, acc core.Account) []int64 {
	t.Helper()
	page, err := h.env.Svc.ListMessages(context.Background(), core.View{FolderID: h.folderOf(acc, "inbox").ID}, "", 50)
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, m := range page.Items {
		ids = append(ids, m.ID)
	}
	if len(ids) == 0 {
		t.Fatal("no message in the inbox")
	}
	return ids
}
