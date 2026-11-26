package core

import (
	"testing"
	"time"
)

// SetRegistryFirstForTest makes the first background registry refresh wait d
// after New, until t ends.
func SetRegistryFirstForTest(t *testing.T, d time.Duration) {
	old := registryFirst
	registryFirst = d
	t.Cleanup(func() { registryFirst = old })
}
