//go:build !windows

package gpg

import "os/exec"

func hideWindow(*exec.Cmd) {}
