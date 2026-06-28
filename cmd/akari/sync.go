package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"sort"
	"time"

	"github.com/jssblck/akari/internal/client/discover"
	"github.com/jssblck/akari/internal/client/resolve"
	"github.com/jssblck/akari/internal/client/syncer"
	"github.com/jssblck/akari/internal/client/upload"
	"github.com/jssblck/akari/internal/config"
)

// runSync performs a single discovery pass and uploads everything new since the
// server's cursor for each file, then prints a tally and exits.
func runSync(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	configPath := fs.String("config", "", "config file path (default: platform config dir)")
	dryRun := fs.Bool("dry-run", false, "resolve and report without uploading")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.LoadClient(*configPath)
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("locate home directory: %w", err)
	}
	machine, _ := os.Hostname()

	files, err := discover.Discover(discover.Roots(cfg, os.Getenv, home))
	if err != nil {
		return fmt.Errorf("discover sessions: %w", err)
	}

	resolver := resolve.New()
	client := upload.New(&http.Client{Timeout: 60 * time.Second}, cfg.ServerURL, cfg.Token)
	sync := syncer.New(resolver, client, machine)

	var (
		uploaded, upToDate, reset, skipped, failed int
		standalone, orphaned                       int
		uploadedBytes                              int64
		skipReasons                                = map[string]int{}
	)
	countKind := func(k resolve.Kind) {
		switch k {
		case resolve.KindStandalone:
			standalone++
		case resolve.KindOrphaned:
			orphaned++
		}
	}

	for _, f := range files {
		if *dryRun {
			res := resolver.Resolve(ctx, f)
			if res.Skipped {
				skipped++
				skipReasons[res.Reason]++
				fmt.Fprintf(os.Stderr, "skip %s: %s\n", f.Path, res.Reason)
				continue
			}
			countKind(res.Kind)
			fmt.Printf("would upload %s -> %s\n", f.Path, syncer.Result{Kind: res.Kind, ProjectKey: res.ProjectKey, Cwd: res.Header.Cwd}.Destination())
			continue
		}

		r := sync.SyncOne(ctx, f)
		switch {
		case r.Skipped:
			skipped++
			skipReasons[r.Reason]++
			fmt.Fprintf(os.Stderr, "skip %s: %s\n", f.Path, r.Reason)
		case r.Err != nil:
			failed++
			fmt.Fprintf(os.Stderr, "error %s: %v\n", f.Path, r.Err)
		case r.Action == upload.ActionUploaded:
			uploaded++
			countKind(r.Kind)
			uploadedBytes += r.UploadedBytes
			fmt.Printf("uploaded %s -> %s (%d bytes, %d messages)\n", f.Path, r.Destination(), r.UploadedBytes, r.MessageCount)
		case r.Action == upload.ActionReset:
			reset++
			countKind(r.Kind)
			uploadedBytes += r.UploadedBytes
			fmt.Printf("reset+uploaded %s -> %s (%d bytes)\n", f.Path, r.Destination(), r.UploadedBytes)
		case r.Action == upload.ActionUpToDate:
			upToDate++
			countKind(r.Kind)
		}
	}

	printSummary(len(files), uploaded, reset, upToDate, skipped, failed, standalone, orphaned, uploadedBytes, skipReasons, *dryRun)
	if failed > 0 {
		return fmt.Errorf("%d file(s) failed to upload", failed)
	}
	return nil
}

func printSummary(total, uploaded, reset, upToDate, skipped, failed, standalone, orphaned int, bytes int64, reasons map[string]int, dryRun bool) {
	if dryRun {
		fmt.Printf("\n%d file(s) discovered, %d skipped (dry run, nothing uploaded)\n", total, skipped)
	} else {
		fmt.Printf("\n%d file(s): %d uploaded, %d reset, %d up to date, %d skipped, %d failed (%d bytes sent)\n",
			total, uploaded, reset, upToDate, skipped, failed, bytes)
	}
	if standalone > 0 || orphaned > 0 {
		fmt.Printf("of which %d standalone, %d orphaned (backed up, no git remote)\n", standalone, orphaned)
	}
	if len(reasons) > 0 {
		keys := make([]string, 0, len(reasons))
		for k := range reasons {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fmt.Println("skips by reason:")
		for _, k := range keys {
			fmt.Printf("  %3d  %s\n", reasons[k], k)
		}
	}
}
