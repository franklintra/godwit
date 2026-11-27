package app

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/egoist/mygo/ui"
	"github.com/godwit-email/godwit/core"
	"github.com/godwit-email/godwit/core/coretest"
	"github.com/godwit-email/godwit/core/events"
	"github.com/godwit-email/godwit/internal/i18n"
	"github.com/godwit-email/godwit/internal/mcpclients"
	"github.com/godwit-email/godwit/uiclient"
)

// gauge counts the calls of one method: how many ran, and the most at once.
type gauge struct{ calls, in, most atomic.Int32 }

func (g *gauge) enter() {
	g.calls.Add(1)
	n := g.in.Add(1)
	for m := g.most.Load(); n > m && !g.most.CompareAndSwap(m, n); m = g.most.Load() {
	}
}

func (g *gauge) leave() { g.in.Add(-1) }

// gate holds the calls that reach it until it opens or their context ends;
// a nil gate lets them through.
type gate struct {
	mu        sync.Mutex
	ch        chan struct{}
	cancelled atomic.Int32 // calls whose context ended while they waited
}

func (g *gate) close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ch = make(chan struct{})
}

func (g *gate) open() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.ch != nil {
		close(g.ch)
		g.ch = nil
	}
}

// pass lets new calls through and keeps those waiting held.
func (g *gate) pass() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ch = nil
}

func (g *gate) wait(ctx context.Context) error {
	g.mu.Lock()
	ch := g.ch
	g.mu.Unlock()
	if ch == nil {
		return nil
	}
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		g.cancelled.Add(1)
		return ctx.Err()
	}
}

// bootSvc is the App's Service with the boot's reads counted, and the views'
// read and the list's held at will.
type bootSvc struct {
	Service
	saved, list, accounts, folders, settings gauge
	savedGate, listGate                      gate
	savedDone                                atomic.Int32 // SavedViews calls that answered
}

func (s *bootSvc) SavedViews(ctx context.Context) ([]core.SavedView, error) {
	s.saved.enter()
	defer s.saved.leave()
	if err := s.savedGate.wait(ctx); err != nil {
		return nil, err
	}
	defer s.savedDone.Add(1)
	return s.Service.SavedViews(ctx)
}

func (s *bootSvc) ListThreads(ctx context.Context, v core.View, cursor string, limit int) (core.ThreadPage, error) {
	s.list.enter()
	defer s.list.leave()
	if err := s.listGate.wait(ctx); err != nil {
		return core.ThreadPage{}, err
	}
	return s.Service.ListThreads(ctx, v, cursor, limit)
}

func (s *bootSvc) ListMessages(ctx context.Context, v core.View, cursor string, limit int) (core.MessagePage, error) {
	s.list.enter()
	defer s.list.leave()
	if err := s.listGate.wait(ctx); err != nil {
		return core.MessagePage{}, err
	}
	return s.Service.ListMessages(ctx, v, cursor, limit)
}

func (s *bootSvc) ListAccounts(ctx context.Context) ([]core.Account, error) {
	s.accounts.enter()
	defer s.accounts.leave()
	return s.Service.ListAccounts(ctx)
}

func (s *bootSvc) ListFolders(ctx context.Context, account int64) ([]core.Folder, error) {
	s.folders.enter()
	defer s.folders.leave()
	return s.Service.ListFolders(ctx, account)
}

func (s *bootSvc) GetSettings(ctx context.Context) (core.Settings, error) {
	s.settings.enter()
	defer s.settings.leave()
	return s.Service.GetSettings(ctx)
}

// bootHarness is a harness whose App talks to the backend through a
// bootSvc, made before the App starts; setup runs on the svc first (to
// close a gate). The accounts each have a message in their inbox.
func bootHarness(t *testing.T, setup func(*bootSvc), emails ...string) (*harness, *bootSvc, []core.Account) {
	t.Helper()
	var accs []core.Account
	h := newHarnessWith(t, []coretest.Option{coretest.WithoutAutoSync()}, func(e *coretest.Env) {
		for i, em := range emails {
			acc := e.AddAccount(t, em)
			e.Deliver(t, acc.ID, "INBOX", coretest.Msg("bob@example.com", em, fmt.Sprintf("hello %d", i), "body"))
			accs = append(accs, acc)
		}
	})
	h.dropUI() // the harness's own App: this one starts on the bootSvc instead
	cl, err := uiclient.Dial(h.sock, "")
	if err != nil {
		t.Fatal(err)
	}
	svc := &bootSvc{Service: cl}
	if setup != nil {
		setup(svc)
	}
	h.ui = cl
	h.app = New(Deps{Svc: svc, Platform: h.plat, Post: h.post, Clock: h.clock, SystemLanguage: "zh-CN", Logf: h.logf})
	h.app.mcpEnv = func() (mcpclients.Env, error) {
		return mcpclients.Env{Home: h.home, OS: "darwin", Temp: h.home, Run: func(string, ...string) error { return nil }}, nil
	}
	h.tt = ui.NewTester(h.app.View, 1280, 820)
	ctx, stop := context.WithCancel(context.Background())
	h.stop = stop
	t.Cleanup(func() { svc.savedGate.open(); svc.listGate.open() }) // before the harness closes the App
	h.app.Start(ctx)
	return h, svc, accs
}

// The list lands, and MailReady holds, while the views' counts are still
// being read: the first list never waits for every view to count.
func TestBootMailReadyBeforeViewCounts(t *testing.T) {
	h, svc, _ := bootHarness(t, func(s *bootSvc) { s.savedGate.close() }, "alice@example.com")
	h.until("the first list", h.app.MailReady)
	if !h.tt.HasText("hello 0") {
		t.Fatalf("the list misses its row; texts %q", h.tt.Texts())
	}
	h.until("the views' read under way", func() bool { return svc.saved.in.Load() == 1 })
	if svc.savedDone.Load() != 0 || !h.app.MailReady() {
		t.Fatalf("SavedViews answered %d, ready %v", svc.savedDone.Load(), h.app.MailReady())
	}
	if !h.app.mail.viewsPending() || h.app.mail.viewsKnown {
		t.Fatal("the views show as read before their read answered")
	}
	svc.savedGate.open()
	h.until("the views", func() bool { return h.app.mail.viewsKnown && !h.app.mail.viewsPending() })
	if !h.app.MailReady() {
		t.Fatal("the views' read took MailReady back")
	}
}

// A burst of mail events while the views' read and a list read are held
// reads one of each at a time, and each at most once more after: never a
// read per event.
func TestBootBurstSingleFlight(t *testing.T) {
	h, svc, accs := bootHarness(t, func(s *bootSvc) { s.savedGate.close() }, "alice@example.com", "bob@example.com")
	h.until("the first list", h.app.MailReady)
	h.until("the views' read under way", func() bool { return svc.saved.in.Load() == 1 })

	svc.listGate.close()
	lists, refreshes := svc.list.calls.Load(), h.app.refreshes
	for i := range 20 {
		h.app.onEvent(events.Event{Type: events.MessagesChanged, AccountID: accs[i%2].ID})
		h.advance(refreshEvery) // each event its own refresh
		if i == 0 {
			h.until("a list read held", func() bool { return svc.list.in.Load() == 1 })
		}
	}
	if got := h.app.refreshes - refreshes; got < 20 {
		t.Fatalf("%d refreshes for 20 spaced events", got)
	}
	h.until("the loads read", func() bool { return !h.app.loads.loading && !h.app.loadAgain })
	if svc.list.most.Load() != 1 || svc.saved.most.Load() != 1 {
		t.Fatalf("at once: %d list reads, %d views' reads", svc.list.most.Load(), svc.saved.most.Load())
	}
	if got := svc.list.calls.Load() - lists; got != 1 || !h.app.mail.listAgain {
		t.Fatalf("%d list reads during the burst, again %v", got, h.app.mail.listAgain)
	}
	if svc.saved.calls.Load() != 1 || svc.savedGate.cancelled.Load() != 0 {
		t.Fatalf("%d views' reads, %d cancelled", svc.saved.calls.Load(), svc.savedGate.cancelled.Load())
	}

	svc.listGate.open()
	svc.savedGate.open()
	h.until("settled", func() bool {
		p := h.app.mail
		return !p.list.loading && !p.listAgain && !p.viewsPending() && !h.app.loads.loading && !h.app.loadAgain
	})
	if got := svc.list.calls.Load() - lists; got != 2 {
		t.Fatalf("%d list reads for the burst, want the held one and one after", got)
	}
	if got := svc.saved.calls.Load(); got != 2 { // the boot's, and one after the mail changed
		t.Fatalf("%d views' reads, want 2", got)
	}
	if svc.list.most.Load() != 1 || svc.saved.most.Load() != 1 {
		t.Fatalf("at once: %d list reads, %d views' reads", svc.list.most.Load(), svc.saved.most.Load())
	}
}

// A view switch drops the list read in flight and cancels its context, so
// the Service stops it; the new view's list lands.
func TestBootViewSwitchCancels(t *testing.T) {
	h, svc, accs := bootHarness(t, nil, "alice@example.com")
	h.until("the first list", func() bool { return h.app.MailReady() && h.app.mail.viewsKnown })
	svc.listGate.close()
	h.app.mail.reload()
	h.until("a list read held", func() bool { return svc.list.in.Load() == 1 })
	svc.listGate.pass() // the held read stays held: only its context lets it go
	h.app.navigate(Route{Page: PageMail, View: core.View{FolderID: h.folderOf(accs[0], "inbox").ID}})
	h.until("the held read cancelled", func() bool { return svc.listGate.cancelled.Load() == 1 })
	h.until("the new view's list", h.app.MailReady)
	if !h.tt.HasText("hello 0") {
		t.Fatalf("the new view misses its row; texts %q", h.tt.Texts())
	}
}

// SyncStarted changes nothing the App reads: the sync status shows it at
// once, and no refresh or load follows.
func TestBootSyncStartedNoLoad(t *testing.T) {
	h, svc, accs := bootHarness(t, nil, "alice@example.com")
	h.until("settled", func() bool {
		return h.app.MailReady() && h.app.mail.viewsKnown && !h.app.mail.viewsPending() && !h.app.loads.loading
	})
	gen, refreshes, accounts := h.app.loads.Gen(), h.app.refreshes, svc.accounts.calls.Load()
	h.app.onEvent(events.Event{Type: events.SyncStarted, AccountID: accs[0].ID})
	for range 4 {
		h.advance(refreshEvery)
	}
	if h.app.loads.Gen() != gen || h.app.refreshes != refreshes || svc.accounts.calls.Load() != accounts {
		t.Fatalf("SyncStarted reloaded: %d loads, %d refreshes, %d ListAccounts",
			h.app.loads.Gen()-gen, h.app.refreshes-refreshes, svc.accounts.calls.Load()-accounts)
	}
	if !h.app.syncing[accs[0].ID] || !h.tt.HasText(i18n.T("shell.sync.syncing")) {
		t.Fatalf("syncing %v; texts %q", h.app.syncing, h.tt.Texts())
	}
}

// A refresh reads the accounts, their folders and the settings once: the
// mail list takes what the App's load read in the same refresh. So does
// the boot.
func TestRefreshReusesLoadReads(t *testing.T) {
	h, svc, accs := bootHarness(t, nil, "alice@example.com", "bob@example.com")
	h.until("the first list", h.app.MailReady)
	if got := svc.accounts.calls.Load(); got != 1 {
		t.Fatalf("%d ListAccounts before the first list", got)
	}
	if got := svc.folders.calls.Load(); got != int32(len(accs)) {
		t.Fatalf("%d ListFolders before the first list, for %d accounts", got, len(accs))
	}
	h.until("settled", func() bool {
		return h.app.mail.viewsKnown && !h.app.mail.viewsPending() && !h.app.loads.loading && !h.app.mail.list.loading
	})
	accounts, folders, settings := svc.accounts.calls.Load(), svc.folders.calls.Load(), svc.settings.calls.Load()
	h.app.onEvent(events.Event{Type: events.MessagesChanged, AccountID: accs[0].ID})
	h.advance(refreshEvery)
	h.until("the refresh read", func() bool {
		return !h.app.loads.loading && !h.app.mail.waitLoad && !h.app.mail.list.loading
	})
	if got := svc.accounts.calls.Load() - accounts; got != 1 {
		t.Fatalf("%d ListAccounts in one refresh", got)
	}
	if got := svc.folders.calls.Load() - folders; got != int32(len(accs)) {
		t.Fatalf("%d ListFolders in one refresh, for %d accounts", got, len(accs))
	}
	if got := svc.settings.calls.Load() - settings; got != 1 {
		t.Fatalf("%d GetSettings in one refresh", got)
	}
	if h.app.unread[accs[0].ID] != 1 || h.app.unread[accs[1].ID] != 1 {
		t.Fatalf("unread %v", h.app.unread)
	}
}
