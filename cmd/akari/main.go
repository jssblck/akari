// Command akari is the session-backup client: it discovers agent sessions,
// resolves each to its git project, and pushes new bytes to an akari server.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/jssblck/akari/internal/version"
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
	case "watch":
		err = runWatch(ctx, os.Args[2:])
	case "daemon":
		err = runDaemon(os.Args[2:])
	case "login":
		err = runLogin(os.Args[2:])
	case "update":
		err = runUpdate(ctx, os.Args[2:])
	case "version", "--version", "-v":
		fmt.Println(version.String())
		return
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
  akari watch [--config PATH]                             watch continuously and upload changes (foreground)
  akari daemon {start|stop|status} [--config PATH]        manage the watch loop as a background process
  akari login --server URL --token TOKEN [--config PATH]  write the client config
  akari update [--check]                                  update to the latest release in place
  akari version                                           print the build version and exit
`)
}
