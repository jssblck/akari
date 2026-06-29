// Command akari is the session-backup client: it discovers agent sessions,
// resolves each to its git project, and pushes new bytes to an akari server.
package main

import (
	"fmt"
	"os"

	"github.com/jssblck/akari/internal/shutdown"
	"github.com/jssblck/akari/internal/version"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	// The first Ctrl-C acks immediately and cancels ctx so the running command
	// stops taking on new work and winds down the unit it is on; a second Ctrl-C
	// exits at once.
	ctx, stop := shutdown.Notify(func() {
		fmt.Fprintln(os.Stderr, "akari: interrupt received, finishing in-flight work (Ctrl-C again to exit now)")
	})
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
  akari sync [--config PATH] [--dry-run] [--time-limit DUR] [--concurrency N] discover and upload new session bytes, then exit
  akari watch [--config PATH]                             watch continuously and upload changes (foreground)
  akari daemon {start|stop|status} [--config PATH]        manage the watch loop as a background process
  akari login --server URL --token TOKEN [--config PATH]  write the client config
  akari update [--check]                                  update to the latest release in place
  akari version                                           print the build version and exit
`)
}
