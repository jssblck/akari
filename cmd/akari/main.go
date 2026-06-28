// Command akari is the session-backup client: it discovers agent sessions,
// resolves each to its git project, and pushes new bytes to an akari server.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "sync":
		err = runSync(ctx, os.Args[2:])
	case "login":
		err = runLogin(os.Args[2:])
	case "help", "-h", "--help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "akari: unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "akari: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `akari - back up agent sessions to an akari server

Usage:
  akari sync [--config PATH] [--dry-run]                  discover and upload new session bytes, then exit
  akari login --server URL --token TOKEN [--config PATH]  write the client config

Watch mode (akari watch) and daemon management arrive in a later milestone.
`)
}
