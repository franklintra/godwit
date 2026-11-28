//go:build linux

package main

import (
	"context"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/godwit-email/godwit/core"
	"github.com/godwit-email/godwit/uiclient"
)

// statusRounds stands in for the sync rounds of accounts that reach their
// server: on Franklin's Mac every account runs a round now and then, and
// each round writes the account's status and sends events the App reloads
// on. The fixture's and a real copy's accounts point at a closed port, so
// their rounds only fail. statusRounds adds n small accounts, each on its
// own loopback IMAP server, and once they are synced runs a SyncNow on
// one of them every interval, in turn: rounds that succeed and find
// nothing new, as an idle account's.
type statusRounds struct {
	srvs   []*loopMail
	ids    []int64
	rounds atomic.Int64
	fails  atomic.Int64
	stop   chan struct{}
	once   sync.Once
	wg     sync.WaitGroup
}

// statusMessages is each status account's mail: small, so its first sync
// ends before the probes start.
const statusMessages = 60

func addStatusAccounts(ctx context.Context, c *uiclient.Client, n int, seed uint64) (*statusRounds, error) {
	s := &statusRounds{stop: make(chan struct{})}
	now := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	for i := range n {
		email := fmt.Sprintf("status%d@perfbench.invalid", i+1)
		plan := accountPlan{Email: email, Messages: statusMessages}
		srv, nmsg, _, err := startLoopMail(email, nil, func(emit func(genMsg)) { genAccount(seed+200+uint64(i), 200+i, plan, now, emit) })
		if err != nil {
			s.close()
			return nil, err
		}
		s.srvs = append(s.srvs, srv)
		acc, err := c.AddAccount(ctx, core.AccountInput{
			Email: email, Name: fmt.Sprintf("Status %d", i+1), Username: email, Password: benchPassword, Sync: &core.SyncRange{},
			Receiving: core.Server{Protocol: "imap", Host: srv.IMAPHost, Port: srv.IMAPPort, Security: core.SecurityNone},
			Sending:   core.Server{Protocol: "smtp", Host: srv.IMAPHost, Port: srv.SMTPPort, Security: core.SecurityNone},
		})
		if err != nil {
			s.close()
			return nil, fmt.Errorf("AddAccount %s: %w", email, err)
		}
		s.ids = append(s.ids, acc.ID)
		t0 := time.Now()
		for storedIn(ctx, c, acc.ID) < nmsg && time.Since(t0) < 3*time.Minute {
			time.Sleep(200 * time.Millisecond)
		}
	}
	return s, nil
}

// storedIn is how many messages an account's folders hold.
func storedIn(ctx context.Context, c *uiclient.Client, accountID int64) int {
	fs, err := c.ListFolders(ctx, accountID)
	if err != nil {
		return 0
	}
	n := 0
	for _, f := range fs {
		n += f.Total
	}
	return n
}

// start runs the rounds on c until close.
func (s *statusRounds) start(ctx context.Context, c *uiclient.Client, every time.Duration) {
	if s == nil || len(s.ids) == 0 {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for k := 0; ; k++ {
			select {
			case <-s.stop:
				return
			case <-time.After(every):
			}
			if err := c.SyncNow(ctx, s.ids[k%len(s.ids)]); err != nil {
				s.fails.Add(1)
				continue
			}
			s.rounds.Add(1)
		}
	}()
}

func (s *statusRounds) close() {
	if s == nil {
		return
	}
	s.once.Do(func() {
		close(s.stop)
		s.wg.Wait()
		for _, m := range s.srvs {
			m.Close()
		}
	})
}

func (s *statusRounds) record(prefix string, minutes float64, rep *report) {
	if s == nil || len(s.ids) == 0 || minutes <= 0 {
		return
	}
	rep.add(prefix+".status_rounds_per_min", "count", "", fmt.Sprintf("sync rounds of the %d status accounts per minute over the probe rounds (SyncNow in turn: rounds that find nothing new)", len(s.ids)), float64(s.rounds.Load())/minutes)
	if f := s.fails.Load(); f > 0 {
		rep.Notes = append(rep.Notes, fmt.Sprintf("%s: %d status rounds failed", prefix, f))
	}
	log.Printf("%s: %d status rounds, %d failed", prefix, s.rounds.Load(), s.fails.Load())
}
