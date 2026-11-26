// Package gpgtest makes throwaway GnuPG keyrings for tests: a fresh
// GNUPGHOME with keys that have no passphrase. Tests that use it skip when
// gpg is not installed.
package gpgtest

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/godwit-email/godwit/internal/gpg"
)

// Keyring is a temporary GNUPGHOME.
type Keyring struct {
	GPG gpg.GPG
	t   testing.TB
}

// New makes an empty keyring, removed (and its gpg-agent stopped) when t
// ends; it skips t when gpg is missing.
func New(t testing.TB) *Keyring {
	t.Helper()
	path, err := gpg.Find("")
	if err != nil {
		t.Skip("gpg not installed")
	}
	// Short: gpg-agent's socket lives in it, and a socket path is short.
	home, err := os.MkdirTemp("", "gpgt")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	k := &Keyring{GPG: gpg.GPG{Path: path, Home: home}, t: t}
	t.Cleanup(func() {
		if conf, err := exec.LookPath("gpgconf"); err == nil {
			cmd := exec.Command(conf, "--homedir", home, "--kill", "all")
			cmd.Run()
		}
		os.RemoveAll(home)
	})
	return k
}

// Gen makes a key for uid ("Name <addr>") that signs and encrypts and
// returns its fingerprint.
func (k *Keyring) Gen(uid string) string {
	k.t.Helper()
	k.gpg("--passphrase", "", "--pinentry-mode", "loopback", "--quick-gen-key", uid, "default", "default", "never")
	keys, err := k.GPG.SecretKeys(context.Background())
	if err != nil {
		k.t.Fatal(err)
	}
	for _, key := range keys {
		for _, u := range key.UserIDs {
			if u.Text == uid {
				return key.Fingerprint
			}
		}
	}
	k.t.Fatalf("key for %q not found", uid)
	return ""
}

// Export is the ASCII-armored public key of fpr.
func (k *Keyring) Export(fpr string) []byte {
	k.t.Helper()
	return k.gpg("--armor", "--export", fpr)
}

// Import adds keys (armored or binary) to the keyring.
func (k *Keyring) Import(keys []byte) {
	k.t.Helper()
	cmd := k.cmd("--import")
	cmd.Stdin = strings.NewReader(string(keys))
	if out, err := cmd.CombinedOutput(); err != nil {
		k.t.Fatalf("gpg --import: %v\n%s", err, out)
	}
}

// Certify makes fpr valid in this keyring with a local signature from its
// own first key, as the user does when they have checked a key.
func (k *Keyring) Certify(fpr string) {
	k.t.Helper()
	k.gpg("--passphrase", "", "--pinentry-mode", "loopback", "--quick-lsign-key", fpr)
}

func (k *Keyring) cmd(args ...string) *exec.Cmd {
	cmd := exec.Command(k.GPG.Path, append([]string{"--batch", "--no-tty", "--disable-dirmngr"}, args...)...)
	cmd.Env = append(os.Environ(), "GNUPGHOME="+k.GPG.Home)
	return cmd
}

func (k *Keyring) gpg(args ...string) []byte {
	k.t.Helper()
	cmd := k.cmd(args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		k.t.Fatalf("gpg %v: %v\n%s", args, err, stderr.String())
	}
	return out
}
