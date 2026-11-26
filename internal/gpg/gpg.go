// Package gpg runs the user's GnuPG (gpg) for OpenPGP: listing keys, signing,
// encrypting, verifying and decrypting. Keys stay in the user's keyring and
// passphrases stay with gpg-agent and its pinentry: nothing secret passes
// through this package or a command line. Data goes in on stdin and comes out
// on stdout; what gpg reports is read from its machine-readable status lines
// (--status-fd), never from its messages, which are translated.
//
// Every run disables gpg's network use (--disable-dirmngr,
// --no-auto-key-retrieve, --auto-key-locate local): no keyserver, WKD or DANE
// lookup happens behind the user's back, whatever their gpg.conf says.
package gpg

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Timeouts for one gpg run. An operation that may ask the user for their
// passphrase (signing, decrypting) waits for them up to UserTimeout.
const (
	QuickTimeout = 30 * time.Second
	UserTimeout  = 5 * time.Minute
)

// MaxOutput bounds what one run may write on stdout: a message is at most
// 25 MB, and armor and encryption add about a third.
const MaxOutput = 40 << 20

// ErrNotFound means no gpg binary was found.
var ErrNotFound = errors.New("gpg not found")

// candidates are where gpg installs outside PATH: a GUI app on macOS starts
// with a bare PATH, so Homebrew's, MacPorts' and GPG Suite's are looked up by name.
var candidates = map[string][]string{
	"darwin":  {"/opt/homebrew/bin/gpg", "/usr/local/bin/gpg", "/usr/local/MacGPG2/bin/gpg", "/opt/local/bin/gpg"},
	"linux":   {"/usr/bin/gpg", "/usr/local/bin/gpg"},
	"windows": {`C:\Program Files\GnuPG\bin\gpg.exe`, `C:\Program Files (x86)\GnuPG\bin\gpg.exe`, `C:\Program Files (x86)\gnupg\bin\gpg.exe`},
}

// Find returns the gpg binary to run: configured when it is set (it must be
// an executable file), else gpg on PATH, else the first of the usual
// install locations that exists. ErrNotFound when there is none.
func Find(configured string) (string, error) {
	if configured != "" {
		if !executable(configured) {
			return "", fmt.Errorf("%w at %s", ErrNotFound, configured)
		}
		return configured, nil
	}
	for _, name := range []string{"gpg", "gpg2"} {
		if p, err := exec.LookPath(name); err == nil {
			if abs, err := filepath.Abs(p); err == nil {
				return abs, nil
			}
		}
	}
	for _, p := range candidates[runtime.GOOS] {
		if executable(p) {
			return p, nil
		}
	}
	return "", ErrNotFound
}

func executable(p string) bool {
	if !filepath.IsAbs(p) {
		return false
	}
	st, err := os.Stat(p)
	if err != nil || st.IsDir() {
		return false
	}
	return runtime.GOOS == "windows" || st.Mode()&0o111 != 0
}

// GPG runs one gpg binary.
type GPG struct {
	Path string // the binary, from Find
	// Home is GNUPGHOME for the runs; "" leaves the environment's (the
	// user's ~/.gnupg). Tests set a throwaway one.
	Home string
}

// Error is a gpg run that failed. Status holds its status lines, Stderr its
// last messages, for logs: they may be translated.
type Error struct {
	Op     string
	Status []Status
	Stderr string
	Err    error
}

func (e *Error) Error() string {
	msg := lastLine(e.Stderr)
	if msg == "" && e.Err != nil {
		msg = e.Err.Error()
	}
	return "gpg " + e.Op + ": " + msg
}

func (e *Error) Unwrap() error { return e.Err }

// Has reports whether the failed run wrote a status line with keyword.
func (e *Error) Has(keyword string) bool {
	for _, s := range e.Status {
		if s.Keyword == keyword {
			return true
		}
	}
	return false
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(lines[i]), "gpg:")); l != "" {
			return l
		}
	}
	return ""
}

// baseArgs come first in every run.
var baseArgs = []string{
	"--batch", "--no-tty", "--status-fd", "2", "--with-colons", "--fixed-list-mode",
	// No network, whatever gpg.conf asks for: verifying a mail on display
	// must not tell a keyserver it was read, and encrypting must not look a
	// recipient up.
	"--disable-dirmngr", "--no-auto-key-retrieve", "--auto-key-locate", "clear,local",
}

// run runs gpg with args, stdin in, and returns stdout, the status lines
// and gpg's other messages.
// A non-zero exit is an *Error unless okExit accepts it (verification exits
// 1 for a bad signature, which the status lines describe).
func (g *GPG) run(ctx context.Context, op string, timeout time.Duration, stdin []byte, okExit func(int) bool, args ...string) ([]byte, []Status, string, error) {
	if g.Path == "" {
		return nil, nil, "", ErrNotFound
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, g.Path, append(append([]string{}, baseArgs...), args...)...)
	cmd.Env = os.Environ()
	if g.Home != "" {
		cmd.Env = append(cmd.Env, "GNUPGHOME="+g.Home)
	}
	cmd.Stdin = bytes.NewReader(stdin)
	var stdout limitedBuffer
	stdout.max = MaxOutput
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = 5 * time.Second
	hideWindow(cmd)
	err := cmd.Run()
	status, messages := splitStatus(stderr.Bytes())
	if stdout.over {
		return nil, status, messages, &Error{Op: op, Status: status, Stderr: messages, Err: errors.New("output too large")}
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ctx.Err() == nil && okExit != nil && okExit(ee.ExitCode()) {
			return stdout.Bytes(), status, messages, nil
		}
		if ctx.Err() != nil {
			err = fmt.Errorf("%w (%v)", ctx.Err(), err)
		}
		return nil, status, messages, &Error{Op: op, Status: status, Stderr: messages, Err: err}
	}
	return stdout.Bytes(), status, messages, nil
}

// limitedBuffer keeps at most max bytes and notes that more came.
type limitedBuffer struct {
	bytes.Buffer
	max  int
	over bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.max {
		b.over = true
		return 0, errors.New("output too large")
	}
	return b.Buffer.Write(p)
}

// Version is gpg's version, e.g. "2.4.8".
func (g *GPG) Version(ctx context.Context) (string, error) {
	out, _, _, err := g.run(ctx, "version", QuickTimeout, nil, nil, "--version")
	if err != nil {
		return "", err
	}
	return parseVersion(out), nil
}

// parseVersion reads the version from --with-colons --version output
// ("cfg:version:2.4.8" with --list-config, or the first line's last word).
func parseVersion(out []byte) string {
	first, _, _ := strings.Cut(string(out), "\n")
	f := strings.Fields(first)
	if len(f) == 0 {
		return ""
	}
	return f[len(f)-1]
}

// tempFile writes data to a new private file and returns its path and a
// cleanup. Only for what is not secret: a detached signature.
func tempFile(data []byte) (string, func(), error) {
	f, err := os.CreateTemp("", "godwit-gpg-*.asc")
	if err != nil {
		return "", nil, err
	}
	name := f.Name()
	cleanup := func() { os.Remove(name) }
	if _, err := f.Write(data); err != nil {
		f.Close()
		cleanup()
		return "", nil, err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", nil, err
	}
	return name, cleanup, nil
}
