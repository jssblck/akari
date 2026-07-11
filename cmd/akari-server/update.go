package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/jssblck/akari/internal/selfupdate"
	"github.com/jssblck/akari/internal/version"
)

// installerURL is the server installer the update shells out to. It is fetched
// from main so an update always runs the current installer logic.
const installerURL = "https://raw.githubusercontent.com/jssblck/akari/main/scripts/install-server.sh"

// runUpdate updates the akari-server binary to the latest release. The server is
// Linux-only, so rather than reimplement the download in Go it shells out to the
// install script (a thin wrapper that already verifies the checksum). The
// install-it-in-place step is safe on Unix: the running process keeps its open
// inode, and a restart picks up the new binary.
func runUpdate(args []string) error {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	check := fs.Bool("check", false, "report whether an update is available without installing it")
	force := fs.Bool("force", false, "reinstall the latest release even if already up to date")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	c := selfupdate.New()
	current := version.String()
	latest, err := c.LatestTag(ctx)
	if err != nil {
		return fmt.Errorf("resolve the latest release: %w", err)
	}
	upToDate, comparable := selfupdate.UpToDate(current, latest)

	if *check {
		printUpdateStatus("akari-server", current, latest, upToDate, comparable)
		return nil
	}
	if upToDate && !*force {
		fmt.Printf("akari-server %s is already the latest release.\n", current)
		return nil
	}

	target, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate the running binary: %w", err)
	}
	target, err = resolveExecutableTarget(target)
	if err != nil {
		return err
	}
	dir := filepath.Dir(target)

	if inContainer() {
		fmt.Fprintln(os.Stderr, "warning: this looks like a container; prefer rebuilding the image and redeploying over updating in place.")
	}

	// Run the installer pinned to the resolved tag, installing over the running
	// binary's directory. The installer downloads, verifies, and installs; it
	// elevates with sudo on its own when the directory needs root.
	cmd := exec.CommandContext(ctx, "sh", "-c", "curl -fsSL "+installerURL+" | sh")
	cmd.Env = append(os.Environ(), "AKARI_INSTALL_DIR="+dir, "AKARI_VERSION="+latest)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("run the installer: %w", err)
	}

	fmt.Printf("akari-server updated from %s to %s.\n", current, latest)
	if _, err := os.Stat("/etc/systemd/system/akari-server.service"); err == nil {
		fmt.Println("Restart the service to apply: sudo systemctl restart akari-server")
	} else {
		fmt.Println("Restart akari-server to use the new version.")
	}
	return nil
}

// inContainer reports whether the process looks like it is running in a
// container, where updating the binary in place is the wrong move (the image
// should be rebuilt and redeployed).
func inContainer() bool {
	_, err := os.Stat("/.dockerenv")
	return err == nil
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
