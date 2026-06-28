package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/jssblck/akari/internal/config"
)

// runLogin writes the client config (server URL and API token) to the config
// file. The token is created out of band (web UI or `akari-server` admin) and
// passed in; akari stores it with owner-only permissions.
func runLogin(args []string) error {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	server := fs.String("server", "", "akari server base URL, e.g. https://akari.example")
	token := fs.String("token", "", "API token (ingest or full scope)")
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

	if err := config.SaveClient(*configPath, cfg); err != nil {
		return err
	}
	path := *configPath
	if path == "" {
		path, _ = config.DefaultClientPath()
	}
	fmt.Fprintf(os.Stderr, "wrote config to %s\n", path)
	return nil
}
