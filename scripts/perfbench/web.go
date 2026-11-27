//go:build linux

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// readerBody is the largest message's body as the backend serves it to the
// reader, saved during the session for the web boot's reader page.
var readerBody []byte

// runWeb runs web/webboot.ts with Bun: bundle sizes of the build's
// web/dist and loads of its reader and Wit pages in Chromium's headless
// shell (Playwright's, in ~/.cache/ms-playwright).
func runWeb(ctx context.Context, c runConfig, rep *report) error {
	script := c.web
	if script == "" {
		script = filepath.Join("scripts", "perfbench", "web", "webboot.ts")
	}
	shells, _ := filepath.Glob(filepath.Join(os.Getenv("HOME"), ".cache", "ms-playwright", "chromium_headless_shell-*", "chrome-linux", "headless_shell"))
	if len(shells) == 0 {
		return fmt.Errorf("no Chromium headless shell in ~/.cache/ms-playwright")
	}
	tmp, err := os.MkdirTemp("", "perfbench-web")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	args := []string{"run", script, "--dist", c.webDir, "--chrome", shells[len(shells)-1], "--runs", fmt.Sprint(c.runs), "--out", filepath.Join(tmp, "web.json")}
	if readerBody != nil {
		bp := filepath.Join(tmp, "body.json")
		os.WriteFile(bp, readerBody, 0o600)
		args = append(args, "--body", bp)
	}
	t0 := time.Now()
	cmd := exec.CommandContext(ctx, "bun", args...)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("webboot: %w", err)
	}
	var res struct {
		Metrics map[string]struct {
			Unit, Better, Desc string
			Samples            []float64
		}
		Notes []string
	}
	b, err := os.ReadFile(filepath.Join(tmp, "web.json"))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, &res); err != nil {
		return err
	}
	for n, m := range res.Metrics {
		rep.add(n, m.Unit, m.Better, m.Desc, m.Samples...)
	}
	rep.Notes = append(rep.Notes, res.Notes...)
	log.Printf("web: %s", time.Since(t0).Round(time.Millisecond))
	return nil
}

func saveReaderBody(id int64, html, text string) {
	readerBody, _ = json.Marshal(map[string]any{"message": id, "html": html, "plain": text})
}
