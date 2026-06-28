package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/jssblck/akari/internal/client/daemon"
)

// runDaemon manages the watch loop as a background process: start, stop, status.
func runDaemon(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: akari daemon {start|stop|status} [--config PATH]")
	}
	sub := args[0]

	fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
	configPath := fs.String("config", "", "config file path (passed to the watch process)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	paths, err := daemon.DefaultPaths()
	if err != nil {
		return err
	}

	switch sub {
	case "start":
		self, err := os.Executable()
		if err != nil {
			return err
		}
		watchArgs := []string{"watch"}
		if *configPath != "" {
			watchArgs = append(watchArgs, "--config", *configPath)
		}
		if err := daemon.Start(self, watchArgs, paths); err != nil {
			return err
		}
		running, pid := daemon.Status(paths)
		if running {
			fmt.Printf("akari watch started (pid %d); logging to %s\n", pid, paths.Logfile)
		}
		return nil

	case "stop":
		if err := daemon.Stop(paths); err != nil {
			return err
		}
		fmt.Println("akari watch stopped")
		return nil

	case "status":
		running, pid := daemon.Status(paths)
		if running {
			fmt.Printf("running (pid %d)\n", pid)
		} else {
			fmt.Println("not running")
		}
		return nil

	default:
		return fmt.Errorf("unknown daemon command %q (want start, stop, or status)", sub)
	}
}
