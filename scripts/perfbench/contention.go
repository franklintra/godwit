//go:build linux

package main

import (
	"context"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/godwit-email/godwit/core"
	"github.com/godwit-email/godwit/uiclient"
)

// runContention reproduces what Franklin's Mac showed: the window opening
// while the backend is busy. A new account's initial sync writes (the
// loopback IMAP server's folder), the menu-bar tray's loads repeat (its
// readSnapshot: GetSettings, ListAccounts, ListFolders per account,
// RecentSignInCodes), a probe times ListAccounts every 250 ms, and the
// interface opens boots times against the running backend. The SQL trace
// splits each call's database time into waiting for the one SQLite
// connection and running.
func runContention(ctx context.Context, o *opener, mail *loopMail, nmsg, boots int, prefix string, rep *report) error {
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
	t0 := time.Now()
	acc, err := setup.AddAccount(ctx, core.AccountInput{
		Email: "sync@perfbench.invalid", Name: "Contention", Username: "sync@perfbench.invalid", Password: benchPassword, Sync: &core.SyncRange{},
		Receiving: core.Server{Protocol: "imap", Host: mail.IMAPHost, Port: mail.IMAPPort, Security: core.SecurityNone},
		Sending:   core.Server{Protocol: "smtp", Host: mail.IMAPHost, Port: mail.SMTPPort, Security: core.SecurityNone},
	})
	if err != nil {
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
	for stored() < min(300, nmsg/10) && time.Since(t0) < 5*time.Minute {
		time.Sleep(100 * time.Millisecond)
	}
	startN, startAt := stored(), time.Now()
	log.Printf("%s: sync is writing (%d stored after %s); opening the interface %d times", prefix, startN, startAt.Sub(t0).Round(time.Millisecond), boots)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	var probeMS []float64
	loop := func(client string, every time.Duration, do func(*uiclient.Client)) error {
		c, _, err := b.dialUI(ctx, time.Minute, nil, client)
		if err != nil {
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer c.Close()
			for {
				select {
				case <-stop:
					return
				default:
				}
				do(c)
				select {
				case <-stop:
					return
				case <-time.After(every):
				}
			}
		}()
		return nil
	}
	tray, _, err := b.dialUI(ctx, time.Minute, nil, "perfbench-tray")
	if err != nil {
		return err
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer tray.Close()
		runTray(ctx, tray, stop)
	}()
	if err := loop("perfbench-probe", 250*time.Millisecond, func(c *uiclient.Client) {
		t := time.Now()
		if _, err := c.ListAccounts(ctx); err == nil {
			mu.Lock()
			probeMS = append(probeMS, ms(time.Since(t)))
			mu.Unlock()
		}
	}); err != nil {
		return err
	}

	type window struct{ from, to time.Time }
	var wins []window
	during := 0
	for i := range boots {
		if i > 0 && stored() >= mail.n {
			rep.Notes = append(rep.Notes, fmt.Sprintf("%s: the sync ended after %d boots; the rest were not run", prefix, i))
			break
		}
		during++
		tr := newRPCTrace()
		from := time.Now()
		c, accept, err := b.dialUI(ctx, time.Minute, tr.wrap, "perfbench-ui")
		if err != nil {
			close(stop)
			wg.Wait()
			return err
		}
		d, err := startUI(ctx, c, o.data)
		if err != nil {
			c.Close()
			close(stop)
			wg.Wait()
			return err
		}
		if err := d.until(10*time.Minute, d.a.MailReady); err != nil {
			d.close()
			c.Close()
			close(stop)
			wg.Wait()
			return fmt.Errorf("boot %d under contention: %w", i+1, err)
		}
		to := time.Now()
		wins = append(wins, window{from, to})
		calls := tr.snapshot(accept)
		rep.add(prefix+".open_ms", "ms", "lower", "the interface opening against the running backend while a sync writes and the tray loads: dial to first list", ms(to.Sub(from)))
		if la := slowest(calls, "ListAccounts"); la > 0 {
			rep.add(prefix+".boot_list_accounts_ms", "ms", "lower", "the boot's ListAccounts under contention", la)
		}
		if sv := slowest(calls, "SavedViews"); sv > 0 {
			rep.add(prefix+".boot_saved_views_ms", "ms", "lower", "the boot's SavedViews under contention", sv)
		}
		log.Printf("%s: boot %d: %.0f ms (%d RPCs)", prefix, i+1, ms(to.Sub(from)), len(calls))
		d.settle(300 * time.Millisecond)
		d.close()
		c.Close()
		time.Sleep(700 * time.Millisecond)
	}
	close(stop)
	wg.Wait()
	endN, endAt := stored(), time.Now()
	rep.add(prefix+".boots_during_sync", "count", "higher", "boots that started while the sync still wrote", float64(during))
	rep.add(prefix+".sync_msgs_per_s", "msg/s", "higher", "the account's initial sync, while the boots ran", float64(endN-startN)/endAt.Sub(startAt).Seconds())
	mu.Lock()
	rep.add(prefix+".probe_list_accounts_ms", "ms", "lower", "ListAccounts every 250 ms on its own connection during the boots", probeMS...)
	mu.Unlock()

	time.Sleep(200 * time.Millisecond) // the trace's last flush
	calls := readSQL(b.sqlPath)
	from, to := wins[0].from, wins[len(wins)-1].to
	sums := sumByCall(calls, from, to)
	var laWait, laExec []float64
	for _, s := range sums {
		if s.Client == "perfbench-probe" && s.Method == "ListAccounts" {
			laWait = append(laWait, ms(s.Wait))
			laExec = append(laExec, ms(s.Exec))
		}
	}
	rep.add(prefix+".probe_list_accounts_sql_wait_ms", "ms", "lower", "per ListAccounts call: its statements' time queued behind other work on the one SQLite connection", laWait...)
	rep.add(prefix+".probe_list_accounts_sql_exec_ms", "ms", "lower", "per ListAccounts call: its statements' time running", laExec...)
	var qWait, qExec []float64
	for _, w := range wins {
		var wait, exec time.Duration
		for _, s := range sumByCall(calls, w.from, w.to) {
			if s.Client == "perfbench-ui" {
				wait += s.Wait
				exec += s.Exec
			}
		}
		rep.add(prefix+".boot_sql_wait_ms", "ms", "lower", "per boot: the interface's statements' time queued for the SQLite connection", ms(wait))
		rep.add(prefix+".boot_sql_exec_ms", "ms", "lower", "per boot: the interface's statements' time running", ms(exec))
		bg, all := busyShare(calls, w.from, w.to)
		rep.add(prefix+".boot_conn_busy_background_pct", "%", "lower", "per boot: share of the boot the connection ran the backend's own work (sync, rules, labels)", bg*100)
		rep.add(prefix+".boot_conn_busy_pct", "%", "lower", "per boot: share of the boot the connection was busy at all", all*100)
	}
	for _, c := range calls {
		if c.Client == "perfbench-ui" && !c.Start.Before(from) && !c.Start.After(to) {
			qWait = append(qWait, ms(c.Wait))
			qExec = append(qExec, ms(c.Exec))
		}
	}
	rep.add(prefix+".ui_query_wait_ms", "ms", "lower", "per SQL statement of the interface's boots: time queued", qWait...)
	rep.add(prefix+".ui_query_exec_ms", "ms", "lower", "per SQL statement of the interface's boots: time running", qExec...)
	for _, bl := range blockers(calls, from, to, 5) {
		rep.Notes = append(rep.Notes, fmt.Sprintf("%s: blocker %.1f ms (%s) %s", prefix, ms(bl.Exec), bl.Kind, bl.Stmt))
	}
	byClient := map[string]time.Duration{}
	for _, c := range calls {
		if !c.Start.Before(from) && !c.Start.After(to) {
			k := c.Client
			if k == "" {
				k = "backend"
			}
			byClient[k] += c.Exec
		}
	}
	keys := make([]string, 0, len(byClient))
	for k := range byClient {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	line := prefix + ": connection time by client during the boots:"
	for _, k := range keys {
		line += fmt.Sprintf(" %s %.0f ms;", k, ms(byClient[k]))
	}
	rep.Notes = append(rep.Notes, line)
	return nil
}
