//go:build !linux

// Command perfbench measures Godwit on Linux only (it reads /proc); see
// README.md.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "perfbench runs on Linux: see scripts/perfbench/README.md")
	os.Exit(2)
}
