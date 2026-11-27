//go:build linux

package main

import (
	"context"
	"errors"
	"os"
	"sync"

	"github.com/egoist/mygo/ui"
	"github.com/godwit-email/godwit/app"
	"github.com/godwit-email/godwit/core"
)

// benchPlatform is app.Platform with nothing behind it: no windows, no
// dialogs, no network. A reader body "paints" at once, as the tests' fake
// does, so the App's open-message path completes.
type benchPlatform struct {
	mu      sync.Mutex
	dataDir string
	temp    string
	post    func(func())
	a       *app.App
	views   map[int64]*benchBody
}

type benchBody struct {
	ui.NativeWebView
	message int64
}

var errNone = errors.New("not on the benchmark platform")

func (p *benchPlatform) OpenURL(string) error                                             { return nil }
func (p *benchPlatform) OpenPath(string) error                                            { return nil }
func (p *benchPlatform) OpenFiles(bool) ([]string, error)                                 { return nil, nil }
func (p *benchPlatform) OpenImage() (string, error)                                       { return "", nil }
func (p *benchPlatform) OpenDirectory() (string, error)                                   { return "", nil }
func (p *benchPlatform) SaveFile(string) (string, error)                                  { return "", nil }
func (p *benchPlatform) Notify(app.Notification) error                                    { return nil }
func (p *benchPlatform) NotificationsSupported() bool                                     { return false }
func (p *benchPlatform) SetOpenAtLogin(bool) error                                        { return nil }
func (p *benchPlatform) OpenAtLogin() bool                                                { return false }
func (p *benchPlatform) SetAppUnread(int)                                                 {}
func (p *benchPlatform) ShowWindow()                                                      {}
func (p *benchPlatform) SetThemeSource(string)                                            {}
func (p *benchPlatform) TempDir() string                                                  { return p.temp }
func (p *benchPlatform) TypeIcon(string, int) ([]byte, error)                             { return nil, errNone }
func (p *benchPlatform) DataDir() (string, bool)                                          { return p.dataDir, true }
func (p *benchPlatform) OpenCompose(int64)                                                {}
func (p *benchPlatform) ComposeOpen(int64) bool                                           { return false }
func (p *benchPlatform) DraftDeleted(int64)                                               {}
func (p *benchPlatform) SettingsChanged(core.Settings)                                    {}
func (p *benchPlatform) ReloadImages(int64, bool)                                         {}
func (p *benchPlatform) BodyGlass(ui.NativeWebView, app.BodyGlassRect, app.BodyGlassRect) {}
func (p *benchPlatform) TranslateBody(int64, map[string]string)                           {}
func (p *benchPlatform) Print(int64)                                                      {}
func (p *benchPlatform) CheckForUpdates()                                                 {}
func (p *benchPlatform) LatestVersion(context.Context) (string, error)                    { return "", nil }
func (p *benchPlatform) UpdatesInstallThemselves() bool                                   { return false }
func (p *benchPlatform) RepoStarred(context.Context) (bool, bool)                         { return false, false }
func (p *benchPlatform) StarRepo(context.Context) error                                   { return nil }
func (p *benchPlatform) SignInCodeBannerSupported() bool                                  { return false }

func (p *benchPlatform) NewSignatureView() (ui.NativeWebView, func(int64), func(func(bool)), func(), error) {
	return nil, nil, nil, nil, errNone
}

func (p *benchPlatform) NewBirdView() (ui.NativeWebView, func(), error) { return nil, nil, errNone }

// NewBodyView makes a stand-in page that reports its height and paint on
// the next drain, as a loaded reader page would.
func (p *benchPlatform) NewBodyView(message int64, _ *core.Message) (ui.NativeWebView, func(bool), error) {
	v := &benchBody{message: message}
	p.mu.Lock()
	if p.views == nil {
		p.views = map[int64]*benchBody{}
	}
	p.views[message] = v
	p.mu.Unlock()
	p.post(func() {
		if p.a != nil {
			p.a.BodyHeight(v, message, 600)
			p.a.BodyPainted(v, message)
		}
	})
	return v, func(bool) {
		p.mu.Lock()
		delete(p.views, message)
		p.mu.Unlock()
	}, nil
}

func newBenchPlatform(dataDir string) *benchPlatform {
	t, _ := os.MkdirTemp("", "perfbench-ui")
	return &benchPlatform{dataDir: dataDir, temp: t}
}
