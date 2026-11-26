//go:build linux

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/godwit-email/godwit/core"
)

// runSyncBench adds an account whose INBOX holds n messages (the fixture's
// mail shapes) on the loopback IMAP server to a fresh backend, and times
// the initial sync until every message is stored. Each run starts from an
// empty data directory.
// syncMail is the loopback IMAP server the sync and contention parts sync
// from: one INBOX of n messages of the fixture's shapes.
func syncMail(n int, seed uint64) (*loopMail, error) {
	plan := accountPlan{Email: "sync@perfbench.invalid", Messages: n}
	srv, _, _, err := startLoopMail(plan.Email, nil, func(emit func(genMsg)) {
		genAccount(seed+99, 99, plan, time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC), func(m genMsg) { m.Folder = "INBOX"; emit(m) })
	})
	return srv, err
}

// runSyncBench adds an account on mail's INBOX of n messages to a fresh
// backend and times the initial sync until every message is stored. Each
// run starts from an empty data directory.
func runSyncBench(ctx context.Context, bin, scratch string, srv *loopMail, runs int, rep *report) error {
	n, raw := srv.n, srv.raw
	plan := accountPlan{Email: "sync@perfbench.invalid", Messages: n}
	for r := range runs {
		dir := filepath.Join(scratch, fmt.Sprintf("sync-%d", r))
		data := filepath.Join(dir, "data")
		os.MkdirAll(data, 0o700)
		key := make([]byte, 32)
		rand.Read(key)
		kf := filepath.Join(dir, "keyring.key")
		os.WriteFile(kf, []byte(hex.EncodeToString(key)), 0o600)
		env := backendEnv(dir, filepath.Join(dir, "keyring.bin"), kf)
		b, err := startBackend(bin, data, env, nil, false)
		if err != nil {
			return err
		}
		c, _, err := b.dialUI(ctx, time.Minute, nil, "perfbench-sync")
		if err != nil {
			b.stop()
			return err
		}
		t0 := time.Now()
		acc, err := c.AddAccount(ctx, core.AccountInput{
			Email: plan.Email, Name: "Sync Bench", Username: plan.Email, Password: benchPassword, IsDefault: true, Sync: &core.SyncRange{},
			Receiving: core.Server{Protocol: "imap", Host: srv.IMAPHost, Port: srv.IMAPPort, Security: core.SecurityNone},
			Sending:   core.Server{Protocol: "smtp", Host: srv.IMAPHost, Port: srv.SMTPPort, Security: core.SecurityNone},
		})
		if err != nil {
			c.Close()
			b.stop()
			return fmt.Errorf("AddAccount: %w", err)
		}
		go c.SyncNow(ctx, acc.ID) // as 立即同步; the worker may already be at it
		got := 0
		deadline := time.Now().Add(10 * time.Minute)
		for got < n && time.Now().Before(deadline) {
			time.Sleep(100 * time.Millisecond)
			fs, err := c.ListFolders(ctx, acc.ID)
			if err != nil {
				continue
			}
			got = 0
			for _, f := range fs {
				got += f.Total
			}
		}
		el := time.Since(t0)
		mem, _ := readMem(b.pid)
		cpu, _ := readCPU(b.pid)
		gc := readGC(b.logPath)
		c.Close()
		b.stop()
		if got < n {
			return fmt.Errorf("sync stored %d of %d messages in 10 minutes", got, n)
		}
		rep.add("sync.msgs_per_s", "msg/s", "higher", fmt.Sprintf("initial sync of a %d-message INBOX from loopback IMAP (AddAccount to all stored)", n), float64(n)/el.Seconds())
		rep.add("sync.mb_per_s", "MB/s", "higher", "raw message bytes synced per second", float64(raw)/1e6/el.Seconds())
		rep.add("sync.seconds", "s", "lower", "", el.Seconds())
		rep.add("sync.peak_rss_mb", "MB", "lower", "backend VmHWM over its life (start + sync)", float64(mem.HWMKB)/1024)
		rep.add("sync.cpu_s", "s", "lower", "backend CPU time (user+sys), start + sync", cpuMS(cpu.CPUTicks)/1000)
		rep.add("sync.gc_max_heap_mb", "MB", "lower", "largest heap at a GC start (gctrace)", float64(gc.MaxHeapMB))
		rep.add("sync.db_mb", "MB", "lower", "godwit.db + WAL after the sync", float64(fileSize(filepath.Join(data, "godwit.db"))+fileSize(filepath.Join(data, "godwit.db-wal")))/1e6)
		log.Printf("sync run %d: %d msgs in %s (%.0f msg/s), peak RSS %.0f MB", r+1, n, el.Round(time.Millisecond), float64(n)/el.Seconds(), float64(mem.HWMKB)/1024)
		os.RemoveAll(dir)
	}
	return nil
}
