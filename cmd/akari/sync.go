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

// defaultTimeLimit caps how long a sync keeps starting new uploads when the
// caller does not pass --time-limit. It bounds an unattended run without cutting
// off a typical backlog; pass --time-limit 0 to remove the cap entirely.
const defaultTimeLimit = 5 * time.Minute

// syncDeadline wraps the shutdown context so a time limit acts as a self-inflicted
// graceful stop: once limit elapses the returned context is cancelled, which the
// sync loop reads the same way it reads a Ctrl-C, so the in-flight upload still
// finishes. A non-positive limit means run until the work is done (or the operator
// interrupts), so only cancellation propagates.
func syncDeadline(ctx context.Context, limit time.Duration) (context.Context, context.CancelFunc) {
	if limit <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, limit)
}

// runSync performs a single discovery pass and uploads everything new since the
// server's cursor for each file, then prints a tally and exits.
func runSync(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	configPath := fs.String("config", "", "config file path (default: platform config dir)")
	dryRun := fs.Bool("dry-run", false, "resolve and report without uploading")
	timeLimitStr := fs.String("time-limit", defaultTimeLimit.String(), "Go duration to keep starting new uploads, e.g. 30s or 5m (0 for no limit); the in-flight upload always finishes")
	if err := fs.Parse(args); err != nil {
		return err
	}
	timeLimit, err := time.ParseDuration(*timeLimitStr)
	if err != nil {
		return fmt.Errorf("invalid --time-limit: %w", err)
	}
	if timeLimit < 0 {
		return fmt.Errorf("invalid --time-limit: must not be negative")
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

	// A time limit is a self-inflicted graceful shutdown: deadline wraps the
	// shutdown context so the loop below reads an elapsed limit exactly as it reads
	// a first Ctrl-C, stopping new files while letting the in-flight one finish.
	deadline, cancel := syncDeadline(ctx, timeLimit)
	defer cancel()

	// work is detached from ctx so the file currently being uploaded finishes to a
	// clean stopping point after a first Ctrl-C or once the time limit elapses; the
	// loop below stops starting new files once deadline is cancelled, and a second
	// Ctrl-C exits the process outright.
	work := context.WithoutCancel(ctx)
	interrupted := false
	for _, f := range files {
		if deadline.Err() != nil {
			interrupted = true
			break
		}
		if *dryRun {
			res := resolver.Resolve(work, f)
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

		r := sync.SyncOne(work, f)
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
	if interrupted {
		// ctx (the bare shutdown context) carries Ctrl-C; if it is still live the
		// loop must have stopped because deadline's own timeout fired instead.
		if ctx.Err() != nil {
			fmt.Fprintf(os.Stderr, "interrupted: stopped before processing every file\n")
		} else {
			fmt.Fprintf(os.Stderr, "time limit reached: stopped before processing every file\n")
		}
	}
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
