//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log"
	"maps"
	"math/rand/v2"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/godwit-email/godwit/core"
	"github.com/godwit-email/godwit/uiclient"
	"github.com/godwit-email/godwit/webui"
)

// Client names of the window part, as the SQL trace tags their calls.
const (
	clientWindow = "perfbench-window" // the App, attached and refreshed by events
	clientUser   = "perfbench-user"   // what the user does in it: the probes
)

// windowViews are the saved views the window part keeps: "franklin" keeps
// only the reply-based one, as Franklin's database had on 2026-11-27 (his
// "À traiter"); "fixture" keeps the fixture's four.
var windowViews = []string{"franklin", "fixture"}

// replyViewNames are the seeded reply-based view's names (core's
// textToHandle) in the languages a fixture may have been made in.
var replyViewNames = []string{"À traiter", "To handle", "待处理"}

// probeSpan is one probe call or sequence, for the SQL trace's split.
type probeSpan struct {
	group    string // view_switch, new_message, avatars, scroll, palette
	from, to time.Time
}

// scrollN mails are opened one after the other in each round, as the user
// goes down the list with the arrow key; scrollDwell is how long each one
// stays on screen once its body shows.
const (
	scrollN     = 10
	scrollDwell = 300 * time.Millisecond
)

// runWindow is what Franklin feels: his window is open while a sync
// writes, and he switches views, opens a new message and scrolls past
// senders' avatars, and scrolls through his mail. A new account's initial
// sync writes (the loopback IMAP folder), the App stays attached and
// reloads on every mail event as the main window does, the tray's loads
// repeat, and between short pauses a user connection does what the window
// asks on a click: ListThreads of the unified inbox, the largest INBOX and
// starred, ViewContents of the reply-based view; NewDraft then the compose
// page's Load (webui, as the compose window calls it); ContactAvatar for a
// dozen list senders at once; then it opens scrollN consecutive mails of
// the unified inbox (openMail: the reader's calls, in its order). The
// first round also runs the palette's Find with nothing typed (it was
// beside NewDraft in the real trace; its contacts scan is the one a
// recipient completion runs). Rounds stop when the sync ends. The SQL
// trace splits each probe's database time into queueing and running, and
// says what held the connection.
func runWindow(ctx context.Context, o *opener, mail *loopMail, meta fixtureMeta, rounds int, views, prefix string, rep *report) error {
	b, err := startBackend(o.bin, o.data, o.env, o.group, true)
	if err != nil {
		return err
	}
	defer func() { b.stop(); b.cleanup() }()
	setup, _, err := b.dialUI(ctx, 10*time.Minute, nil, "perfbench-setup")
	if err != nil {
		return err
	}
	defer setup.Close()
	reply, err := shapeForWindow(ctx, setup, meta, views)
	if err != nil {
		return fmt.Errorf("shaping the copy: %w", err)
	}

	wc, _, err := b.dialUI(ctx, time.Minute, nil, clientWindow)
	if err != nil {
		return err
	}
	defer wc.Close()
	d, err := startUI(ctx, wc, o.data)
	if err != nil {
		return err
	}
	if err := d.until(10*time.Minute, d.a.MailReady); err != nil {
		d.close()
		return fmt.Errorf("window: first list: %w", err)
	}
	d.settle(time.Second)
	user, _, err := b.dialUI(ctx, time.Minute, nil, clientUser)
	if err != nil {
		d.close()
		return err
	}
	defer user.Close()
	senders, err := listSenders(ctx, user, 12)
	if err != nil {
		d.close()
		return err
	}
	scroll, err := listThreadIDs(ctx, user, rounds*scrollN)
	if err != nil {
		d.close()
		return err
	}

	t0 := time.Now()
	acc, err := setup.AddAccount(ctx, core.AccountInput{
		Email: "sync@perfbench.invalid", Name: "Window", Username: "sync@perfbench.invalid", Password: benchPassword, Sync: &core.SyncRange{},
		Receiving: core.Server{Protocol: "imap", Host: mail.IMAPHost, Port: mail.IMAPPort, Security: core.SecurityNone},
		Sending:   core.Server{Protocol: "smtp", Host: mail.IMAPHost, Port: mail.SMTPPort, Security: core.SecurityNone},
	})
	if err != nil {
		d.close()
		return fmt.Errorf("AddAccount: %w", err)
	}
	stored := func() int {
		fs, err := setup.ListFolders(ctx, acc.ID)
		if err != nil {
			return 0
		}
		n := 0
		for _, f := range fs {
			n += f.Total
		}
		return n
	}

	// The window runs on its own goroutine from here: drained and drawn
	// as a window in front, so it reloads on the sync's events.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			d.drain()
			if d.tt.FramePending() {
				d.frame()
			}
			time.Sleep(16 * time.Millisecond)
		}
	}()
	tray, _, err := b.dialUI(ctx, time.Minute, nil, "perfbench-tray")
	if err != nil {
		close(stop)
		wg.Wait()
		d.close()
		return err
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer tray.Close()
		for {
			trayRound(ctx, tray)
			select {
			case <-stop:
				return
			case <-time.After(time.Second):
			}
		}
	}()
	end := func() {
		close(stop)
		wg.Wait()
		d.close()
	}

	for stored() < min(300, mail.n/10) && time.Since(t0) < 5*time.Minute {
		time.Sleep(100 * time.Millisecond)
	}
	startN, startAt := stored(), time.Now()
	log.Printf("%s: sync is writing (%d stored after %s); %d probe rounds with the window open", prefix, startN, startAt.Sub(t0).Round(time.Millisecond), rounds)

	big := meta.Accounts[0]
	comp := webui.NewCompose()
	comp.SetService(user)
	win := 0
	page := webui.NewComposePage(comp, webui.PageDeps{WinOf: func(context.Context) (int, bool) { return win, true }})
	pause := func() { time.Sleep(500 * time.Millisecond) } // the user reads what came

	var spans []probeSpan
	type roundSpan struct{ from, to time.Time }
	var roundSpans []roundSpan
	timed := func(group, name, desc string, do func() error) error {
		from := time.Now()
		err := do()
		to := time.Now()
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		spans = append(spans, probeSpan{group, from, to})
		rep.add(prefix+"."+name+"_ms", "ms", "lower", desc, ms(to.Sub(from)))
		if group == "view_switch" {
			rep.add(prefix+".view_switch_ms", "ms", "lower", "every view switch of the rounds together (the four lists and views)", ms(to.Sub(from)))
		}
		return nil
	}
	list := func(name, desc string, v core.View) error {
		err := timed("view_switch", "view."+name, desc, func() error {
			_, err := user.ListThreads(ctx, v, "", pageSize)
			return err
		})
		pause()
		return err
	}
	done := 0
	for r := range rounds {
		if r > 0 && stored() >= mail.n {
			rep.Notes = append(rep.Notes, fmt.Sprintf("%s: the sync ended after %d rounds; the rest were not run", prefix, r))
			break
		}
		from := time.Now()
		if err := list("unified_inbox", "during sync, window open: ListThreads of the unified inbox, first page (a click on Inbox)", core.View{Unified: "inbox"}); err != nil {
			end()
			return err
		}
		if err := list("inbox_big", "during sync, window open: ListThreads of the largest account's INBOX (a click on a big folder)", core.View{FolderID: big.Inbox}); err != nil {
			end()
			return err
		}
		if err := list("starred", "during sync, window open: ListThreads of unified starred", core.View{Unified: "starred"}); err != nil {
			end()
			return err
		}
		if err := timed("view_switch", "view.saved_reply", "during sync, window open: ViewContents of the reply-based saved view (Franklin's À traiter)", func() error {
			_, err := user.ViewContents(ctx, reply, pageSize)
			return err
		}); err != nil {
			end()
			return err
		}
		pause()

		// a new message: the main window's NewDraft, then the compose
		// window's page loads (webui.ComposePage.Load)
		win++
		var draft core.Draft
		var nd, ld time.Duration
		if err := timed("new_message", "new_message", "during sync: a new message, as the user waits for it: NewDraft then the compose page's Load", func() error {
			t := time.Now()
			var err error
			if draft, err = user.NewDraft(ctx, big.ID, "new", 0); err != nil {
				return fmt.Errorf("NewDraft: %w", err)
			}
			nd = time.Since(t)
			comp.Attach(win, draft.ID)
			t = time.Now()
			if _, err := page.Load(ctx); err != nil {
				return fmt.Errorf("compose Load: %w", err)
			}
			ld = time.Since(t)
			return nil
		}); err != nil {
			end()
			return err
		}
		rep.add(prefix+".new_draft_ms", "ms", "lower", "during sync: NewDraft (the main window, before the compose window opens)", ms(nd))
		rep.add(prefix+".compose_load_ms", "ms", "lower", "during sync: the compose page's Load (GetDraft, accounts, identities, settings, Wit, signature, OpenPGP)", ms(ld))
		comp.Release(win) // the window closes: an empty draft goes
		pause()

		// avatars: the list's rows ask for their senders' at once
		var avMu sync.Mutex
		if err := timed("avatars", "avatars", fmt.Sprintf("during sync: ContactAvatar for %d list senders at once, until the last answers", len(senders)), func() error {
			var g sync.WaitGroup
			var first error
			for _, s := range senders {
				g.Add(1)
				go func() {
					defer g.Done()
					t := time.Now()
					_, err := user.ContactAvatar(ctx, s)
					avMu.Lock()
					defer avMu.Unlock()
					if err != nil && first == nil {
						first = err
					}
					rep.add(prefix+".avatar_call_ms", "ms", "lower", "during sync: one ContactAvatar call among them", ms(time.Since(t)))
				}()
			}
			g.Wait()
			return first
		}); err != nil {
			end()
			return err
		}
		pause()

		// scrolling: the next scrollN mails of the unified inbox, each
		// opened once the one before shows; what a mail asks after its
		// body (deadline, avatar, unsubscribe...) overlaps the next ones
		sFrom := time.Now()
		var trail sync.WaitGroup
		var opened []mailOpen
		var openMu sync.Mutex
		for _, tid := range scroll[r*scrollN : (r+1)*scrollN] {
			trail.Add(1)
			th, shown, err := openMail(ctx, user, tid, func(m mailOpen) {
				openMu.Lock()
				opened = append(opened, m)
				openMu.Unlock()
				trail.Done()
			})
			if err != nil {
				trail.Done()
				trail.Wait()
				end()
				return fmt.Errorf("open thread %d: %w", tid, err)
			}
			rep.add(prefix+".open.thread_ms", "ms", "lower", "during sync, scrolling: selection to the thread read (GetThread, ThreadLabels)", ms(th))
			rep.add(prefix+".message_open_ms", "ms", "lower", "during sync, scrolling: selection to the newest message's body read, when the reader switches (GetThread, ThreadLabels, then GetMessageLazyImages, ImagesAllowed)", ms(shown))
			time.Sleep(scrollDwell)
		}
		trail.Wait()
		sTo := time.Now()
		spans = append(spans, probeSpan{"scroll", sFrom, sTo})
		rep.add(prefix+".scroll_ms", "ms", "lower", fmt.Sprintf("during sync: %d consecutive mails opened, %s each once shown, until the last call of the last answered", scrollN, scrollDwell), ms(sTo.Sub(sFrom)))
		failed := map[string]int{}
		for _, m := range opened {
			rep.add(prefix+".open.all_ms", "ms", "lower", "during sync, scrolling: selection to the last of the mail's calls answered (deadline, cards, avatar, unsubscribe, OpenPGP...)", ms(m.all))
			rep.add(prefix+".open.thread_deadline_ms", "ms", "lower", "during sync, scrolling: the mail's ThreadDeadline (loadWhy: reply statuses of the whole mailbox)", ms(m.deadline))
			for _, f := range m.failed {
				failed[f]++
			}
		}
		for _, k := range slices.Sorted(maps.Keys(failed)) {
			rep.Notes = append(rep.Notes, fmt.Sprintf("%s: round %d: %s failed for %d of %d mails", prefix, r+1, k, failed[k], len(opened)))
		}

		if r == 0 {
			// the palette with nothing typed (Find, kind all, a few of each)
			pause()
			if err := timed("palette", "palette_find", "during sync, first round only: the palette's Find with nothing typed (recent mail, contacts, attachments: the contacts scan CompleteContacts runs too)", func() error {
				_, err := user.Find(ctx, core.FindQuery{Limit: 3})
				return err
			}); err != nil {
				end()
				return err
			}
		}
		roundSpans = append(roundSpans, roundSpan{from, time.Now()})
		done++
		log.Printf("%s: round %d done in %s (%d stored)", prefix, r+1, time.Since(from).Round(time.Millisecond), stored())
		time.Sleep(time.Second)
	}
	end()
	endN, endAt := stored(), time.Now()
	rep.add(prefix+".rounds_during_sync", "count", "higher", "probe rounds that started while the sync still wrote", float64(done))
	rep.add(prefix+".sync_msgs_per_s", "msg/s", "higher", "the account's initial sync while the window was open and probed", float64(endN-startN)/endAt.Sub(startAt).Seconds())
	if len(roundSpans) == 0 {
		return fmt.Errorf("no round ran during the sync")
	}

	time.Sleep(200 * time.Millisecond) // the trace's last flush
	calls := readSQL(b.sqlPath)
	waits := map[string][2]time.Duration{}
	for _, sp := range spans {
		var wait, exec time.Duration
		for _, c := range calls {
			if c.Client == clientUser && !c.Start.Before(sp.from) && !c.Start.After(sp.to) {
				wait += c.Wait
				exec += c.Exec
			}
		}
		rep.add(prefix+"."+sp.group+"_sql_wait_ms", "ms", "lower", "per "+sp.group+" probe: its statements' time queued for the one SQLite connection", ms(wait))
		rep.add(prefix+"."+sp.group+"_sql_exec_ms", "ms", "lower", "per "+sp.group+" probe: its statements' time running", ms(exec))
		w := waits[sp.group]
		waits[sp.group] = [2]time.Duration{w[0] + wait, w[1] + exec}
	}
	for _, rs := range roundSpans {
		bg, all := busyShare(calls, rs.from, rs.to)
		rep.add(prefix+".conn_busy_pct", "%", "lower", "per round: share of the round the one SQLite connection was busy at all", all*100)
		rep.add(prefix+".conn_busy_background_pct", "%", "lower", "per round: share of the round the connection ran the backend's own work (sync, rules, labels)", bg*100)
		sh := clientShare(calls, rs.from, rs.to)
		rep.add(prefix+".conn_busy_window_pct", "%", "lower", "per round: share of the round the connection ran the window's own reloads (SavedViews, counts, lists, avatars)", sh[clientWindow]*100)
		rep.add(prefix+".conn_busy_user_pct", "%", "lower", "per round: share of the round the connection ran the probes' statements", sh[clientUser]*100)
	}
	from, to := roundSpans[0].from, roundSpans[len(roundSpans)-1].to
	line := fmt.Sprintf("%s: connection time by client over the rounds (%s):", prefix, to.Sub(from).Round(time.Millisecond))
	sh := clientShare(calls, from, to)
	keys := make([]string, 0, len(sh))
	for k := range sh {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		line += fmt.Sprintf(" %s %.0f%%;", k, sh[k]*100)
	}
	rep.Notes = append(rep.Notes, line)
	for _, g := range []string{"view_switch", "new_message", "avatars", "scroll", "palette"} {
		w := waits[g]
		if w[0]+w[1] > 0 {
			rep.Notes = append(rep.Notes, fmt.Sprintf("%s: %s probes: SQL queued %.0f ms, running %.0f ms in all", prefix, g, ms(w[0]), ms(w[1])))
		}
	}
	// the window's own reloads: on Franklin's Mac its SavedViews recount
	// ran every ~3.5 s during sync (17 + 35 statements in a minute)
	byMethod := map[string]int{}
	for _, s := range sumByCall(calls, from, to) {
		if s.Client == clientWindow {
			byMethod[s.Method]++
		}
	}
	mins := to.Sub(from).Minutes()
	rep.add(prefix+".window_saved_views_per_min", "count", "lower", "the window's SavedViews calls per minute over the rounds (its recount after mail events)", float64(byMethod["SavedViews"])/mins)
	rep.add(prefix+".window_list_threads_per_min", "count", "lower", "the window's ListThreads calls per minute over the rounds (its list reloads after mail events)", float64(byMethod["ListThreads"])/mins)
	wl := fmt.Sprintf("%s: the window's calls over the rounds:", prefix)
	for _, k := range slices.Sorted(maps.Keys(byMethod)) {
		wl += fmt.Sprintf(" %s %d;", k, byMethod[k])
	}
	rep.Notes = append(rep.Notes, wl)
	for _, s := range topStatements(calls, from, to, 12) {
		rep.Notes = append(rep.Notes, fmt.Sprintf("%s: held the connection %.0f ms in %d runs (%s) %s", prefix, ms(s.exec), s.n, s.who, s.stmt))
	}
	return nil
}

// trayRound is one of the menu-bar tray's loads (its readSnapshot).
func trayRound(ctx context.Context, c *uiclient.Client) {
	c.GetSettings(ctx)
	accs, _ := c.ListAccounts(ctx)
	for _, a := range accs {
		c.ListFolders(ctx, a.ID)
	}
	c.RecentSignInCodes(ctx, time.Now())
}

// shapeForWindow makes the run's copy look like Franklin's database where
// the fixture differs: each account gets a signature with two images (his
// signatures carry images: the compose path reads signature_images), and
// with views "franklin" only the reply-based view stays. It returns that
// view's id, saving one when the fixture has none.
func shapeForWindow(ctx context.Context, c *uiclient.Client, meta fixtureMeta, views string) (int64, error) {
	var reply int64
	for _, v := range meta.Views {
		if slices.Contains(replyViewNames, v.Name) {
			reply = v.ID
		}
	}
	if reply == 0 {
		var err error
		if reply, err = c.SaveView(ctx, replyView()); err != nil {
			return 0, fmt.Errorf("SaveView: %w", err)
		}
	}
	if views == "franklin" {
		for _, v := range meta.Views {
			if v.ID != reply {
				if err := c.DeleteView(ctx, v.ID); err != nil {
					return 0, fmt.Errorf("DeleteView %q: %w", v.Name, err)
				}
			}
		}
	}
	for i, a := range meta.Accounts {
		sig, err := c.CreateSignature(ctx, fmt.Sprintf("Bench %d", i+1))
		if err != nil {
			return 0, fmt.Errorf("CreateSignature: %w", err)
		}
		html := core.SignatureHTMLPrefix + fmt.Sprintf(`<p>Bench User %d<br>Some Company, Some Street 1, 1000 City</p><p><img src="%s" width="160"> <img src="%s" width="300"></p>`,
			i+1, noisePNG(uint64(i)*2+1, 160, 160), noisePNG(uint64(i)*2+2, 300, 200))
		if _, err := c.UpdateSignature(ctx, sig.ID, core.SignaturePatch{HTML: &html}); err != nil {
			return 0, fmt.Errorf("UpdateSignature: %w", err)
		}
		id := sig.ID
		if _, err := c.UpdateAccount(ctx, a.ID, core.AccountPatch{SignatureID: &id}); err != nil {
			return 0, fmt.Errorf("UpdateAccount: %w", err)
		}
	}
	return reply, nil
}

// replyView is core's seeded 待处理 (toHandleDoc), for a fixture without it.
func replyView() core.SavedView {
	unreplied := core.RuleCondition{Field: core.FieldReply, Op: core.OpIs, Value: core.ViewReplyUnreplied}
	waiting := core.RuleCondition{Field: core.FieldReply, Op: core.OpOlderThan, Value: "3"}
	var leave []core.RuleCondition
	for _, slug := range []string{"otp", "notice", "newsletter"} {
		leave = append(leave, core.RuleCondition{Field: core.FieldLabel, Op: core.OpNotContains, Value: slug})
	}
	return core.SavedView{Name: replyViewNames[0], Icon: "chat-3", Doc: core.SavedViewDoc{Version: core.SavedViewVersion, Match: core.MatchAll,
		Conditions: []core.RuleCondition{{Match: core.MatchAny, Group: []core.RuleCondition{unreplied, waiting}}, {Match: core.MatchAll, Group: leave}},
		Groups: []core.ViewGroup{
			{ID: "reply", Name: "À répondre", Match: core.MatchAll, Conditions: []core.RuleCondition{unreplied}},
			{ID: "wait", Name: "À relancer", Match: core.MatchAll, Conditions: []core.RuleCondition{waiting}},
		}}}
}

// noisePNG is a w×h PNG of seeded noise as a data: URL: it does not
// compress, so it weighs about 4·w·h bytes, as a photo or logo would.
func noisePNG(seed uint64, w, h int) string {
	r := rand.New(rand.NewPCG(seed, 716))
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			v := r.Uint32()
			img.SetNRGBA(x, y, color.NRGBA{uint8(v), uint8(v >> 8), uint8(v >> 16), 255})
		}
	}
	var b bytes.Buffer
	png.Encode(&b, img)
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(b.Bytes())
}

// listSenders are the senders of the unified inbox's first page, up to n
// distinct: the rows whose avatars the list asks for.
func listSenders(ctx context.Context, c *uiclient.Client, n int) ([]string, error) {
	p, err := c.ListThreads(ctx, core.View{Unified: "inbox"}, "", pageSize)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, t := range p.Items {
		if t.From != "" && !slices.Contains(out, t.From) {
			out = append(out, t.From)
		}
		if len(out) == n {
			break
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("the unified inbox has no senders")
	}
	return out, nil
}

// clientShare is the share of [from, to] the connection ran each client's
// statements ("backend" for the backend's own work).
func clientShare(calls []sqlCall, from, to time.Time) map[string]float64 {
	span := to.Sub(from)
	out := map[string]float64{}
	if span <= 0 {
		return out
	}
	for _, c := range calls {
		s, e := c.End.Add(-c.Exec), c.End
		if s.Before(from) {
			s = from
		}
		if e.After(to) {
			e = to
		}
		if e.Before(s) {
			continue
		}
		k := c.Client
		if c.Method == "" {
			k = "backend"
		}
		out[k] += float64(e.Sub(s)) / float64(span)
	}
	return out
}

type stmtSum struct {
	stmt, who string
	exec      time.Duration
	n         int
}

// topStatements are the statements (by their first 100 characters and who
// ran them) that held the connection longest in [from, to], summed.
func topStatements(calls []sqlCall, from, to time.Time, n int) []stmtSum {
	by := map[string]*stmtSum{}
	for _, c := range calls {
		if c.End.Before(from) || c.Start.After(to) {
			continue
		}
		who := "backend"
		if c.Method != "" {
			who = c.Client + "/" + c.Method
		}
		st := c.Stmt
		if len(st) > 100 {
			st = st[:100]
		}
		st = strings.TrimSpace(st)
		k := who + "\x00" + st
		s := by[k]
		if s == nil {
			s = &stmtSum{stmt: st, who: who}
			by[k] = s
		}
		s.exec += c.Exec
		s.n++
	}
	out := make([]stmtSum, 0, len(by))
	for _, s := range by {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].exec > out[j].exec })
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// listThreadIDs are the unified inbox's first n threads, in list order.
func listThreadIDs(ctx context.Context, c *uiclient.Client, n int) ([]int64, error) {
	var out []int64
	cur := ""
	for len(out) < n {
		p, err := c.ListThreads(ctx, core.View{Unified: "inbox"}, cur, pageSize)
		if err != nil {
			return nil, err
		}
		for _, t := range p.Items {
			out = append(out, t.ID)
		}
		if p.NextCursor == "" || len(p.Items) == 0 {
			break
		}
		cur = p.NextCursor
	}
	if len(out) < n {
		return nil, fmt.Errorf("the unified inbox has %d threads, %d wanted", len(out), n)
	}
	return out[:n], nil
}

// mailOpen is what one opened mail cost, once every call it made answered.
type mailOpen struct {
	all, deadline time.Duration // selection to the last answer; its ThreadDeadline alone
	failed        []string      // the background calls that failed
}

// openMail asks what the reader asks when a row is selected
// (app/reader.go), in its order and with its concurrency: load reads the
// thread (GetThread, ThreadLabels); once read, expand(newest) starts its
// plugin cards and tags and loadBody (GetMessageLazyImages, then
// ImagesAllowed), markRead flags the unread \Seen (SetFlags), startAI reads
// the skill menu (ListSkills, ListActions) and loadWhy the reply deadline
// (ThreadDeadline; ViewWhy only in a saved view); once the body is read,
// loadAvatar (ImagesAllowed, ListAddressBooks, SenderAvatar),
// loadUnsubscribe and loadOpenPGP. It returns when the body is read, as
// the reader switches then; the rest goes on, and done gets its times once
// all answered. Models are off in the fixture: no summary, gist or quick
// replies.
func openMail(ctx context.Context, c *uiclient.Client, tid int64, done func(mailOpen)) (thread, shown time.Duration, err error) {
	t0 := time.Now()
	th, err := c.GetThread(ctx, tid)
	if err != nil {
		return 0, 0, fmt.Errorf("GetThread: %w", err)
	}
	if _, err := c.ThreadLabels(ctx, []int64{tid}); err != nil {
		return 0, 0, fmt.Errorf("ThreadLabels: %w", err)
	}
	thread = time.Since(t0)
	if len(th.Messages) == 0 {
		return 0, 0, fmt.Errorf("thread %d has no messages", tid)
	}
	last := th.Messages[len(th.Messages)-1].ID
	var unread []int64
	for _, m := range th.Messages {
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
	call("PluginCards", func() error { _, err := c.PluginCards(ctx, last); return err })
	call("PluginTags", func() error { _, err := c.PluginTags(ctx, last); return err })
	body := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := c.GetMessageLazyImages(ctx, last, false)
		if err == nil {
			_, err = c.ImagesAllowed(ctx, last)
		}
		body <- err
		if err != nil {
			return
		}
		call("avatar", func() error {
			trusted, err := c.ImagesAllowed(ctx, last)
			if err != nil {
				return err
			}
			if !trusted {
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
			av, err := c.SenderAvatar(ctx, last, false)
			if err == nil && trusted && len(av.Data) > 0 && av.Source != "carddav" {
				_, err = c.ImagesAllowed(ctx, last)
			}
			return err
		})
		call("UnsubscribeMethod", func() error { _, err := c.UnsubscribeMethod(ctx, last); return err })
		call("OpenPGPMessage", func() error { _, err := c.OpenPGPMessage(ctx, last); return err })
	}()
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
	err = <-body
	shown = time.Since(t0)
	if err != nil {
		wg.Wait()
		return thread, shown, fmt.Errorf("body of %d: %w", last, err)
	}
	go func() {
		wg.Wait()
		done(mailOpen{all: time.Since(t0), deadline: deadline, failed: failed})
	}()
	return thread, shown, nil
}
