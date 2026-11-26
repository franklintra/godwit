//go:build windows

package gpg

import (
	"os/exec"
	"syscall"
)

// hideWindow keeps gpg from opening a console window.
func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
}
