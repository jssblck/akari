package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/jssblck/akari/internal/selfupdate"
	"github.com/jssblck/akari/internal/version"
)

// runUpdate replaces the running akari client with the latest release. It is a
// native updater: it resolves the latest release, downloads and checksum-verifies
// the archive for this OS and architecture, and swaps the binary in place, with
// no dependency on a shell or curl.
func runUpdate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	check := fs.Bool("check", false, "report whether an update is available without installing it")
	force := fs.Bool("force", false, "reinstall the latest release even if already up to date")
	if err := fs.Parse(args); err != nil {
		return err
	}

	c := selfupdate.New()
	current := version.String()
	latest, err := c.LatestTag(ctx)
	if err != nil {
		return fmt.Errorf("resolve the latest release: %w", err)
	}
	upToDate, comparable := selfupdate.UpToDate(current, latest)

	if *check {
		printUpdateStatus("akari", current, latest, upToDate, comparable)
		return nil
	}
	if upToDate && !*force {
		fmt.Printf("akari %s is already the latest release.\n", current)
		return nil
	}

	target, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate the running binary: %w", err)
	}
	// Replace the real file, not a symlink that points at it.
	if resolved, err := filepath.EvalSymlinks(target); err == nil {
		target = resolved
	}

	// Clear any leftover from a previous Windows update before staging the next.
	selfupdate.CleanupOld(target)

	// Stage the new binary next to the target so Replace's rename stays on one
	// filesystem.
	tmp := filepath.Join(filepath.Dir(target), fmt.Sprintf(".akari-update-%d", os.Getpid()))
	if err := c.Fetch(ctx, "akari", latest, runtime.GOOS, runtime.GOARCH, tmp); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := selfupdate.Replace(target, tmp); err != nil {
		os.Remove(tmp)
		return err
	}

	fmt.Printf("akari updated from %s to %s.\n", current, latest)
	fmt.Println("Restart any running akari watch or daemon to use the new version.")
	return nil
}

// printUpdateStatus reports the result of an update check for binName.
func printUpdateStatus(binName, current, latest string, upToDate, comparable bool) {
	switch {
	case !comparable:
		fmt.Printf("%s is a development build (%s); the latest release is %s.\n", binName, current, latest)
		fmt.Printf("Run `%s update` to install it.\n", binName)
	case upToDate:
		fmt.Printf("%s %s is up to date (latest release %s).\n", binName, current, latest)
	default:
		fmt.Printf("update available: %s (current %s).\n", latest, current)
		fmt.Printf("Run `%s update` to install it.\n", binName)
	}
}
