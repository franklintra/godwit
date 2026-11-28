//go:build !unix && !windows

package core

func freeBytes(string) (uint64, bool) { return 0, false }
