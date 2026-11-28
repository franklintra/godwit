package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/godwit-email/godwit/core/store"
	"github.com/godwit-email/godwit/ownerlock"
)

// forkUndo is the hidden command `godwit fork-undo <rewrite> [--data-dir
// dir]`: it reverses one fork rewrite (FORK.md, "Fork rewrites"), so stock
// Godwit can read the database again. For "bodies" it puts every mail body
// back inline and restores upstream's triggers (ADR 0053). Godwit must not
// run on the data directory. The fork's next start moves the bodies again.
func forkUndo(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("fork-undo", stderr)
	dataDir := fs.String("data-dir", "", "")
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: godwit fork-undo <rewrite> [--data-dir <dir>]")
		return 2
	}
	name, rest := args[0], args[1:]
	if code, ok := parse(fs, rest); !ok {
		return code
	}
	r, ok := store.Rewrite(name)
	if !ok || fs.NArg() != 0 {
		fmt.Fprintf(stderr, "godwit: fork-undo: no fork rewrite %q\n", name)
		return 2
	}
	dir, err := dataDirOr(*dataDir)
	if err != nil {
		fmt.Fprintln(stderr, "godwit:", err)
		return 1
	}
	path := filepath.Join(dir, "godwit.db")
	if _, err := os.Stat(path); err != nil {
		fmt.Fprintln(stderr, "godwit: fork-undo:", err)
		return 1
	}
	release, err := ownerlock.Acquire(dir)
	if err != nil {
		fmt.Fprintf(stderr, "godwit: fork-undo: quit Godwit first: %v\n", err)
		return 1
	}
	defer release()
	db, err := store.Open(ctx, path)
	if err != nil {
		fmt.Fprintln(stderr, "godwit: fork-undo:", err)
		return 1
	}
	start, last := time.Now(), time.Now()
	err = r.Undo(ctx, db, func(n int) {
		if time.Since(last) > 5*time.Second {
			fmt.Fprintf(stdout, "%d rows back\n", n)
			last = time.Now()
		}
	})
	if cerr := db.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		fmt.Fprintln(stderr, "godwit: fork-undo:", err, "(run it again to go on)")
		return 1
	}
	fmt.Fprintf(stdout, "fork rewrite %s undone in %s; stock Godwit reads this database now\n", name, time.Since(start).Round(time.Millisecond))
	return 0
}
