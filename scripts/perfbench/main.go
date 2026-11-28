//go:build linux

// Command perfbench measures Godwit's opening time, memory, idle cost, hot
// paths, sync throughput and web boot on a synthetic mailbox shaped like a
// heavy user's (8 accounts, 45,000 messages, 2.6 GB). See README.md.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "fixture":
		err = cmdFixture(ctx, os.Args[2:])
	case "run":
		err = cmdRun(ctx, os.Args[2:])
	case "compare":
		err = cmdCompare(os.Args[2:])
	case "show":
		err = cmdShow(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		log.Fatal(err)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage:
  perfbench fixture [-root DIR] [-scale 1] [-godwit BIN]   make the shared fixture if missing
  perfbench run -godwit BIN [-label NAME] [-commit SHA] [-quick] [-only open,mem,hot,idle,pressure,contention,sync,web] [-mem 350M] [-web-dist DIR]
  perfbench compare A.json B.json                          delta table, B against A
  perfbench show A.json                                    the readable table of one run
`)
	os.Exit(2)
}

func cmdCompare(args []string) error {
	if len(args) != 2 {
		usage()
	}
	a, err := loadReport(args[0])
	if err != nil {
		return err
	}
	b, err := loadReport(args[1])
	if err != nil {
		return err
	}
	printCompare(os.Stdout, a, b)
	return nil
}

func cmdShow(args []string) error {
	if len(args) != 1 {
		usage()
	}
	r, err := loadReport(args[0])
	if err != nil {
		return err
	}
	printTable(os.Stdout, r)
	return nil
}
