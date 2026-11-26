package gpg_test

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/godwit-email/godwit/internal/gpg/gpgtest"
)

func clearsign(t *testing.T, k *gpgtest.Keyring, fpr, text string) []byte {
	t.Helper()
	cmd := exec.Command(k.GPG.Path, "--batch", "--no-tty", "--clearsign", "--local-user", fpr)
	cmd.Env = append(os.Environ(), "GNUPGHOME="+k.GPG.Home)
	cmd.Stdin = strings.NewReader(text)
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	return out
}
