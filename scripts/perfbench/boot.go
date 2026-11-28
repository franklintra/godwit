//go:build linux

package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/godwit-email/godwit/app"
	"github.com/godwit-email/godwit/uiclient"
)

// uiDriver runs Godwit's own interface (app.App, the native main window's
// logic) without a window: mygo's headless Tester renders its frames, and
// what the App posts to its UI thread runs on the driver's goroutine. Over
// a real ui connection to the backend, the App makes exactly the calls it
// makes at launch.
type uiDriver struct {
	a      *app.App
	tt     *ui.Tester
	plat   *benchPlatform
	cancel context.CancelFunc

	mu    sync.Mutex
	queue []func()

	frames   int
	frameDur time.Duration
}

func (d *uiDriver) post(f func()) {
	d.mu.Lock()
	d.queue = append(d.queue, f)
	d.mu.Unlock()
}

func (d *uiDriver) drain() {
	for {
		d.mu.Lock()
		q := d.queue
		d.queue = nil
		d.mu.Unlock()
		if len(q) == 0 {
			return
		}
		for _, f := range q {
			f()
		}
	}
}

func (d *uiDriver) frame() {
	t0 := time.Now()
	d.tt.Frame()
	d.frameDur += time.Since(t0)
	d.frames++
}

// startUI makes the App as gui.go does once its backend answered: settings
// first (GetSettings), then app.New, the window's view, Start, foreground.
func startUI(ctx context.Context, c *uiclient.Client, dataDir string) (*uiDriver, error) {
	d := &uiDriver{plat: newBenchPlatform(dataDir)}
	d.plat.post = d.post
	st, err := c.GetSettings(ctx)
	if err != nil {
		return nil, fmt.Errorf("GetSettings: %w", err)
	}
	d.a = app.New(app.Deps{Svc: c, Platform: d.plat, Settings: st, Post: d.post, SystemLanguage: "en",
		Logf: func(f string, args ...any) { log.Printf("app: "+f, args...) }, Quit: func() {}})
	d.plat.a = d.a
	d.tt = ui.NewTester(d.a.View, 1280, 820)
	actx, cancel := context.WithCancel(context.Background())
	d.cancel = cancel
	d.a.Start(actx)
	d.a.SetForeground(true)
	return d, nil
}

// until drains and renders frames until cond holds, or fails after timeout.
func (d *uiDriver) until(timeout time.Duration, cond func() bool) error {
	deadline := time.Now().Add(timeout)
	for {
		d.drain()
		d.frame()
		if cond() {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s", timeout)
		}
		time.Sleep(500 * time.Microsecond)
	}
}

// settle keeps the UI running for d, so follow-up loads (avatars, counts,
// labels after the list) are issued and answered.
func (d *uiDriver) settle(dur time.Duration) {
	end := time.Now().Add(dur)
	for time.Now().Before(end) {
		d.drain()
		d.frame()
		time.Sleep(2 * time.Millisecond)
	}
}

func (d *uiDriver) close() {
	d.a.Close()
	d.cancel()
	os.RemoveAll(d.plat.temp)
}
