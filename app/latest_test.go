package app

import (
	"context"
	"errors"
	"testing"
	"time"
)

// uiApp is an App with only what do needs: no engine, no socket. step runs
// the next callback posted to its UI thread.
func uiApp(t *testing.T) (a *App, step func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	posted := make(chan func(), 16)
	a = &App{ctx: ctx, post: func(f func()) { posted <- f }}
	return a, func() {
		t.Helper()
		select {
		case f := <-posted:
			f()
		case <-time.After(5 * time.Second):
			t.Fatal("nothing posted to the UI thread")
		}
	}
}

// An older run's result is dropped once a newer one started, though it
// finishes last, and its context is cancelled: it would otherwise hold the
// Service's connection ahead of the newer one.
func TestLatestDropsStale(t *testing.T) {
	a, step := uiApp(t)
	var l latest
	cancelled := make(chan bool, 1)
	var got []string
	l.Run(a, func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			cancelled <- true
		case <-time.After(5 * time.Second):
			cancelled <- false
		}
		return ctx.Err()
	}, func(error) { got = append(got, "old") })
	l.Run(a, func(context.Context) error { return nil }, func(error) { got = append(got, "new") })
	if !l.loading {
		t.Fatal("not loading while the new run is in flight")
	}
	if !<-cancelled {
		t.Fatal("the old run's context was not cancelled")
	}
	step()
	step()
	if l.loading || len(got) != 1 || got[0] != "new" {
		t.Fatalf("after both runs: loading %v, got %q", l.loading, got)
	}
}

// RunKeep's run is dropped by a later Run like any other, but left to
// finish: what it writes completes.
func TestLatestRunKeepFinishes(t *testing.T) {
	a, step := uiApp(t)
	var l latest
	release, cancelled := make(chan struct{}), make(chan bool, 1)
	var got []string
	l.RunKeep(a, func(ctx context.Context) error {
		<-release
		cancelled <- ctx.Err() != nil
		return nil
	}, func(error) { got = append(got, "kept") })
	l.Run(a, func(context.Context) error { return nil }, func(error) { got = append(got, "new") })
	step()
	if l.loading || len(got) != 1 || got[0] != "new" {
		t.Fatalf("after the new run: loading %v, got %q", l.loading, got)
	}
	close(release)
	if <-cancelled {
		t.Fatal("the kept run's context was cancelled")
	}
	step()
	if len(got) != 1 {
		t.Fatalf("the kept run landed: %q", got)
	}
	// A run after a Run is cancelled again: keep belongs to RunKeep's run alone.
	done := make(chan bool, 1)
	l.Run(a, func(ctx context.Context) error {
		<-ctx.Done()
		done <- true
		return ctx.Err()
	}, func(error) {})
	l.Run(a, func(context.Context) error { return nil }, func(error) {})
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a Run after RunKeep's was not cancelled")
	}
	step()
	step()
}

// Stop drops the run in flight and cancels it; Also, part of the run before,
// is dropped with it; Run then starts afresh and lands.
func TestLatestStopAndRestart(t *testing.T) {
	a, step := uiApp(t)
	var l latest
	started := make(chan struct{})
	var got []string
	l.Run(a, func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}, func(error) { got = append(got, "stopped") })
	l.Also(a, func(context.Context) error { return nil }, func(error) { got = append(got, "also") })
	<-started
	gen := l.Gen()
	l.Stop()
	if l.loading || l.Gen() <= gen {
		t.Fatalf("after Stop: loading %v, gen %d (was %d)", l.loading, l.Gen(), gen)
	}
	step()
	step()
	if len(got) != 0 {
		t.Fatalf("landed after Stop: %q", got)
	}
	l.Run(a, func(context.Context) error { return nil }, func(error) { got = append(got, "again") })
	l.Also(a, func(context.Context) error { return nil }, func(error) { got = append(got, "also") })
	if !l.loading {
		t.Fatal("not loading")
	}
	step()
	step()
	if len(got) != 2 || l.loading {
		t.Fatalf("after the restart: loading %v, got %q", l.loading, got)
	}
}

// done hears the newest run's error; a failure ends loading like a success.
func TestLatestError(t *testing.T) {
	a, step := uiApp(t)
	var l latest
	boom := errors.New("boom")
	var got error
	l.Run(a, func(context.Context) error { return boom }, func(err error) { got = err })
	step()
	if !errors.Is(got, boom) || l.loading {
		t.Fatalf("got %v, loading %v", got, l.loading)
	}
}
