package main

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/godwit-email/godwit/core"
	"github.com/godwit-email/godwit/core/events"
	"github.com/godwit-email/godwit/uiclient"
)

// This file replays what the interface's reader and the backend's tray ask
// (app/reader.go, cmd/godwit/headless.go), for the window and contention
// parts: one OpenThread before a mail shows, the calls beside it after, the
// next and previous mails' opens read ahead, and the tray's coalesced reads.

// prefetcher replays the reader's prefetch: the opens of the rows beside
// the one shown, at most two, read once it shows; an open taken is shown
// with no call when its read has finished, and the reads still under way
// stop. Events drop what they may have changed, as prefetchEvent does.
type prefetcher struct {
	c   *uiclient.Client
	mu  sync.Mutex
	pre map[int64]*benchPrefetch // by thread
}

type benchPrefetch struct {
	cancel  context.CancelFunc
	ot      *core.OpenedThread // set once read
	changed []int64            // messages events named while it was read
}

func newPrefetcher(c *uiclient.Client) *prefetcher {
	return &prefetcher{c: c, pre: map[int64]*benchPrefetch{}}
}

// want reads the opens of threads, dropping the others.
func (p *prefetcher) want(ctx context.Context, threads ...int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for tid, pf := range p.pre {
		if !slices.Contains(threads, tid) {
			pf.cancel()
			delete(p.pre, tid)
		}
	}
	for _, tid := range threads {
		if tid == 0 || p.pre[tid] != nil {
			continue
		}
		rctx, cancel := context.WithCancel(ctx)
		pf := &benchPrefetch{cancel: cancel}
		p.pre[tid] = pf
		go func() {
			ot, err := p.c.OpenThread(rctx, tid, 0)
			p.mu.Lock()
			defer p.mu.Unlock()
			if p.pre[tid] != pf {
				return
			}
			if err != nil || slices.ContainsFunc(ot.Thread.Messages, func(m core.MessageSummary) bool { return slices.Contains(pf.changed, m.ID) }) {
				delete(p.pre, tid)
				return
			}
			pf.ot = &ot
		}()
	}
}

// take is thread tid's open if its read finished; the reads under way stop.
func (p *prefetcher) take(tid int64) (core.OpenedThread, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pf := p.pre[tid]
	delete(p.pre, tid)
	for id, o := range p.pre {
		if o.ot == nil {
			o.cancel()
			delete(p.pre, id)
		}
	}
	if pf == nil || pf.ot == nil {
		if pf != nil {
			pf.cancel()
		}
		return core.OpenedThread{}, false
	}
	return *pf.ot, true
}

// event drops what ev may have changed.
func (p *prefetcher) event(ev events.Event) {
	switch ev.Type {
	case events.SyncStarted, events.SyncFailed, events.RunsChanged, events.MCPSendsChanged, events.ImportProgress,
		events.OutboxChanged, events.DraftConflict, events.SkillsChanged, events.ModelsChanged, events.MCPChanged:
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for tid, pf := range p.pre {
		switch {
		case len(ev.ThreadIDs) == 0 && len(ev.MessageIDs) == 0, slices.Contains(ev.ThreadIDs, tid):
			pf.cancel()
			delete(p.pre, tid)
		case pf.ot == nil:
			pf.changed = append(pf.changed, ev.MessageIDs...)
		case slices.ContainsFunc(pf.ot.Thread.Messages, func(m core.MessageSummary) bool { return slices.Contains(ev.MessageIDs, m.ID) }):
			pf.cancel()
			delete(p.pre, tid)
		}
	}
}

// follow feeds the user client's events to p until ctx ends.
func (p *prefetcher) follow(ctx context.Context) {
	ch, stop := p.c.Subscribe()
	go func() {
		defer stop()
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-ch:
				if !ok {
					return
				}
				p.event(ev)
			}
		}
	}()
}

// mailOpen is what one opened mail cost, once every call it made answered.
type mailOpen struct {
	all, deadline time.Duration // selection to the last answer; its ThreadDeadline alone
	failed        []string      // the background calls that failed
	prefetched    bool          // shown from a prefetched open, with no call
}

// openMail asks what the reader asks when a row is selected
// (app/reader.go): its open (OpenThread), or nothing when pf holds it read
// ahead. Once the body shows (the page paints within a frame or two), the
// calls beside it start: the read flag (SetFlags), the skill menu
// (ListSkills, ListActions), the reply date (ThreadDeadline; ViewWhy only in
// a saved view), cards and tags, how it unsubscribes, OpenPGP and the
// avatar (SenderAvatar, the open's ImagesAllowed reused; ListAddressBooks
// first when images ask). It returns when the body is read, as the reader
// switches then; the rest goes on, and done gets its times once all
// answered. Models are off: no summary, gist or quick replies.
func openMail(ctx context.Context, c *uiclient.Client, pf *prefetcher, tid int64, done func(mailOpen)) (thread, shown time.Duration, err error) {
	t0 := time.Now()
	ot, hit := pf.take(tid)
	if !hit {
		if ot, err = c.OpenThread(ctx, tid, 0); err != nil {
			return 0, 0, err
		}
	}
	thread = time.Since(t0)
	shown = thread
	if len(ot.Thread.Messages) == 0 || ot.MessageID == 0 {
		return thread, shown, errNoMessages(tid)
	}
	last := ot.MessageID
	var unread []int64
	for _, m := range ot.Thread.Messages {
		if !slices.Contains(m.Flags, `\Seen`) {
			unread = append(unread, m.ID)
		}
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	var failed []string
	var deadline time.Duration
	call := func(name string, f func() error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := f(); err != nil {
				mu.Lock()
				failed = append(failed, name)
				mu.Unlock()
			}
		}()
	}
	if len(unread) > 0 {
		call("SetFlags", func() error { return c.SetFlags(ctx, unread, []string{`\Seen`}, nil) })
	}
	call("ListSkills", func() error { _, err := c.ListSkills(ctx); return err })
	call("ListActions", func() error { _, err := c.ListActions(ctx, "message"); return err })
	call("ThreadDeadline", func() error {
		t := time.Now()
		_, err := c.ThreadDeadline(ctx, tid)
		mu.Lock()
		deadline = time.Since(t)
		mu.Unlock()
		return err
	})
	call("PluginCards", func() error { _, err := c.PluginCards(ctx, last); return err })
	call("PluginTags", func() error { _, err := c.PluginTags(ctx, last); return err })
	call("avatar", func() error {
		if !ot.ImagesAllowed {
			books, err := c.ListAddressBooks(ctx)
			if err != nil {
				return err
			}
			if !slices.ContainsFunc(books, func(ab core.AddressBook) bool {
				return slices.ContainsFunc(ab.Collections, func(col core.AddressBookCollection) bool { return col.Enabled && col.CardCount > 0 })
			}) {
				return nil
			}
		}
		_, err := c.SenderAvatar(ctx, last, false)
		return err
	})
	call("UnsubscribeMethod", func() error { _, err := c.UnsubscribeMethod(ctx, last); return err })
	call("OpenPGPMessage", func() error { _, err := c.OpenPGPMessage(ctx, last); return err })
	go func() {
		wg.Wait()
		done(mailOpen{all: time.Since(t0), deadline: deadline, failed: failed, prefetched: hit})
	}()
	return thread, shown, nil
}

type errNoMessages int64

func (e errNoMessages) Error() string { return "the thread has no message to open" }

// trayEvery is the backend tray's shortest time between two reads
// (trayLoadEvery in cmd/godwit/headless.go).
const trayEvery = 2 * time.Second

// runTray replays the backend's tray (cmd/godwit/headless.go) on c until
// stop closes: a full read at start, then on mail events and settings at
// most one read per trayEvery, of the settings when they changed, of the
// inbox count of the accounts the events name (every account for an event
// naming none), and of the sign-in codes after new mail.
func runTray(ctx context.Context, c *uiclient.Client, stop <-chan struct{}) {
	ch, unsub := c.Subscribe()
	defer unsub()
	type owed struct {
		settings, codes, all bool
		accounts             map[int64]bool
	}
	known := map[int64]bool{}
	read := func(d owed) {
		if d.settings {
			c.GetSettings(ctx)
		}
		if d.all || len(d.accounts) > 0 {
			accs, _ := c.ListAccounts(ctx)
			for _, a := range accs {
				if d.all || d.accounts[a.ID] || !known[a.ID] {
					c.ListFolders(ctx, a.ID)
					known[a.ID] = true
				}
			}
		}
		if d.codes {
			c.RecentSignInCodes(ctx, time.Now())
		}
	}
	read(owed{settings: true, all: true, codes: true})
	last := time.Now()
	var d owed
	var due <-chan time.Time
	for {
		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			mail := func(codes bool) {
				d.codes = d.codes || codes
				if ev.AccountID == 0 {
					d.all = true
				} else {
					if d.accounts == nil {
						d.accounts = map[int64]bool{}
					}
					d.accounts[ev.AccountID] = true
				}
			}
			switch ev.Type {
			case events.MessageReceived:
				mail(true)
			case events.MessagesChanged, events.SyncFinished:
				mail(false)
			case events.SettingsChanged:
				d.settings, d.all = true, true
			case events.PluginsChanged:
				d.codes = true
			default:
				continue
			}
			if due == nil {
				due = time.After(max(0, time.Until(last.Add(trayEvery))))
			}
		case <-due:
			due = nil
			last = time.Now()
			read(d)
			d = owed{}
		}
	}
}
