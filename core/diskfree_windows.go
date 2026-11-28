package core

import "golang.org/x/sys/windows"

// freeBytes is the space the caller may still use on the volume that holds
// dir; ok is false when it cannot be read.
func freeBytes(dir string) (n uint64, ok bool) {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, false
	}
	var avail, total, free uint64
	if windows.GetDiskFreeSpaceEx(p, &avail, &total, &free) != nil {
		return 0, false
	}
	return avail, true
}
