package main

import (
	"fmt"
	"io"

	"github.com/godwit-email/godwit/app"
)

// openCmd is `godwit open TARGET`: it shows TARGET in Godwit's window the
// way a second launch does (godwit --open, OnSecondInstance). It starts an
// interface process for TARGET, which hands it to the running interface and
// exits, or becomes the interface when none runs (and starts the backend).
// TARGET is what app.ValidOpenTarget takes: draft:<id>, message:<id>,
// compose, window, account:<id>, or a mailto: or godwit: link.
//
// Only Godwit's default data directory has a single interface: on another
// (--data-dir) each `godwit open` starts an interface of its own.
func openCmd(args []string, stderr io.Writer) int {
	fs := newFlagSet("godwit open", stderr)
	dataDir := fs.String("data-dir", "", "")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	if fs.NArg() != 1 || !app.ValidOpenTarget(fs.Arg(0)) {
		fmt.Fprintln(stderr, "godwit open: give one TARGET: draft:ID, message:ID, compose, window, a mailto: or a godwit: link")
		fs.Usage()
		return 2
	}
	dir, err := dataDirOr(*dataDir)
	if err != nil {
		fmt.Fprintln(stderr, "godwit:", err)
		return 1
	}
	cmd, err := uiCmd(dir, fs.Arg(0))
	if err == nil {
		err = cmd.Start()
	}
	if err != nil {
		fmt.Fprintln(stderr, "godwit open:", err)
		return 1
	}
	if cmd.Args[0] == "/usr/bin/open" { // LaunchServices returns once the app has the launch
		if err := cmd.Wait(); err != nil {
			fmt.Fprintln(stderr, "godwit open:", err)
			return 1
		}
		return 0
	}
	_ = cmd.Process.Release() // it may become the interface: it outlives this command
	return 0
}
