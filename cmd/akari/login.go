package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/jssblck/akari/internal/config"
)

// isFlagSet reports whether a flag was actually present on the command line, as
// opposed to sitting at its zero-value default. login uses it to tell "clear the
// machine name" (--machine="" passed explicitly) apart from "leave it untouched"
// (--machine omitted entirely).
func isFlagSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}

// runLogin writes the client config (server URL and API token) to the config
// file. The token is created out of band (web UI or `akari-server` admin) and
// passed in; akari stores it with owner-only permissions.
func runLogin(args []string) error {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	server := fs.String("server", "", "akari server base URL, e.g. https://akari.example")
	token := fs.String("token", "", "API token (ingest or full scope)")
	machine := fs.String("machine", "", "logical machine name to report for this client's sessions (default: OS hostname). Give an ephemeral fleet a stable identity, e.g. ci or sandbox-pool; AKARI_MACHINE overrides it per run")
	configPath := fs.String("config", "", "config file path (default: platform config dir)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *server == "" || *token == "" {
		return fmt.Errorf("both --server and --token are required")
	}

	// Preserve any existing extra roots and excludes rather than clobbering them.
	// ReadClient distinguishes a missing file (fine, start blank) from a corrupt
	// one (refuse to overwrite and lose recoverable content).
	cfg, _, err := config.ReadClient(*configPath)
	if err != nil {
		return err
	}
	cfg.ServerURL = *server
	cfg.Token = *token
	// Only overwrite the machine when the flag is given, so re-running login to
	// rotate a token leaves an existing machine identity in place. Passing
	// --machine with an empty value is the way to clear it back to the hostname.
	if isFlagSet(fs, "machine") {
		cfg.Machine = strings.TrimSpace(*machine)
	}

	if err := config.SaveClient(*configPath, cfg); err != nil {
		return err
	}
	path := *configPath
	if path == "" {
		path, _ = config.DefaultClientPath()
	}
	fmt.Fprintf(os.Stderr, "wrote config to %s\n", path)
	if cfg.Machine != "" {
		fmt.Fprintf(os.Stderr, "reporting sessions as machine %q\n", cfg.Machine)
	}
	return nil
}
