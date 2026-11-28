//go:build unix

package core

import "golang.org/x/sys/unix"

// freeBytes is the space an unprivileged writer may still use on the volume
// that holds dir; ok is false when it cannot be read.
func freeBytes(dir string) (n uint64, ok bool) {
	var st unix.Statfs_t
	if unix.Statfs(dir, &st) != nil {
		return 0, false
	}
	return uint64(st.Bavail) * uint64(st.Bsize), true
}
