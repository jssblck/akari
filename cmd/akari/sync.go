package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"sort"
	"sync"
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

// maxDefaultConcurrency caps the default file-level parallelism. Each file
// already fans its own body uploads out under the client's shared adaptive
// limiter and CPU-bounded compression encoder, so the file loop only needs
// enough parallelism to overlap the per-file announce and existence-check
// round-trips. A modest cap keeps that overlap without letting the file count
// multiply against the per-file fan-out.
const maxDefaultConcurrency = 8

// defaultConcurrency picks the default for --concurrency: enough to overlap
// per-file latency on a typical machine, but capped so the file loop stays
// modest relative to each file's own internal parallelism.
func defaultConcurrency() int {
	return min(runtime.NumCPU(), maxDefaultConcurrency)
}

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
	concurrency := fs.Int("concurrency", defaultConcurrency(), "max files to sync in parallel; each file already parallelizes its own body uploads under a shared limiter, so keep this modest")
	finalize := fs.Bool("finalize", false, "treat every session as terminal: flush a Codex session's trailing turn now instead of waiting for the idle settle window. Use on ephemeral hosts (CI, cloud sandboxes) whose window never elapses before teardown")
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
	if *concurrency < 1 {
		return fmt.Errorf("invalid --concurrency: must be at least 1")
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

	files, err := discover.Discover(discover.Roots(cfg, os.Getenv, home), discover.NewExcluder(cfg.Excludes))
	if err != nil {
		return fmt.Errorf("discover sessions: %w", err)
	}

	resolver := resolve.New()
	client := upload.New(&http.Client{Timeout: 60 * time.Second}, cfg.ServerURL, cfg.Token)
	sync := syncer.New(resolver, client, machine, *finalize)

	// A time limit is a self-inflicted graceful shutdown: deadline wraps the
	// shutdown context so the driver reads an elapsed limit exactly as it reads a
	// first Ctrl-C, stopping new files while letting in-flight ones finish.
	deadline, cancel := syncDeadline(ctx, timeLimit)
	defer cancel()

	// work is detached from ctx so files currently being uploaded finish to a clean
	// stopping point after a first Ctrl-C or once the time limit elapses; the driver
	// stops starting new files once deadline is cancelled, and a second Ctrl-C exits
	// the process outright.
	work := context.WithoutCancel(ctx)

	// run is the per-file unit of work: a dry run only resolves and reports, a real
	// sync resolves then pushes the gap. Both are safe to call from many goroutines
	// at once (the resolver guards its cache, the uploader serializes per path and
	// shares its limiter and encoder across files), so syncAll fans them out.
	run := func(c context.Context, f discover.File) outcome {
		if *dryRun {
			res := resolver.Resolve(c, f)
			return outcome{resolve: &res}
		}
		r := sync.SyncOne(c, f)
		return outcome{sync: &r}
	}

	sum, interrupted := syncAll(work, deadline, files, *concurrency, run)

	printSummary(len(files), sum, *dryRun)
	if interrupted {
		// ctx (the bare shutdown context) carries Ctrl-C; if it is still live the
		// driver must have stopped because deadline's own timeout fired instead.
		if ctx.Err() != nil {
			fmt.Fprintf(os.Stderr, "interrupted: stopped before processing every file\n")
		} else {
			fmt.Fprintf(os.Stderr, "time limit reached: stopped before processing every file\n")
		}
	}
	if sum.failed > 0 {
		return fmt.Errorf("%d file(s) failed to upload", sum.failed)
	}
	return nil
}

// outcome carries one file's finished work from a sync goroutine to the single
// goroutine that folds results. Exactly one field is set: resolve for a dry run,
// sync for a real upload. Pointers keep a zero outcome (neither path taken)
// distinguishable and cheap to pass on the channel.
type outcome struct {
	sync    *syncer.Result
	resolve *resolve.Result
}

// summary accumulates per-file outcomes into the final tally. It is deliberately
// not safe for concurrent use: syncAll folds every outcome on one goroutine, so
// the counts and the printed lines stay consistent without any locking.
type summary struct {
	uploaded, upToDate, reset, skipped, failed int
	standalone, orphaned                       int
	uploadedBytes                              int64
	skipReasons                                map[string]int
}

func newSummary() *summary {
	return &summary{skipReasons: map[string]int{}}
}

func (s *summary) countKind(k resolve.Kind) {
	switch k {
	case resolve.KindStandalone:
		s.standalone++
	case resolve.KindOrphaned:
		s.orphaned++
	}
}

// fold records one outcome in the summary and returns the line to print for it,
// if any, and whether that line belongs on stderr (skips and errors) rather than
// stdout (successes). An up-to-date file prints nothing.
func (s *summary) fold(o outcome) (line string, stderr bool) {
	switch {
	case o.resolve != nil:
		return s.foldResolve(*o.resolve)
	case o.sync != nil:
		return s.foldSync(*o.sync)
	}
	return "", false
}

// foldResolve folds a dry-run resolution: it never uploads, so it only tallies
// skips and per-kind counts and reports what a real run would have sent.
func (s *summary) foldResolve(res resolve.Result) (line string, stderr bool) {
	if res.Skipped {
		s.skipped++
		s.skipReasons[res.Reason]++
		return fmt.Sprintf("skip %s: %s", res.File.Path, res.Reason), true
	}
	s.countKind(res.Kind)
	dest := syncer.Result{Kind: res.Kind, ProjectKey: res.ProjectKey, Cwd: res.Header.Cwd}.Destination()
	return fmt.Sprintf("would upload %s -> %s", res.File.Path, dest), false
}

// foldSync folds a real sync result into the running tally.
func (s *summary) foldSync(r syncer.Result) (line string, stderr bool) {
	switch {
	case r.Skipped:
		s.skipped++
		s.skipReasons[r.Reason]++
		return fmt.Sprintf("skip %s: %s", r.File.Path, r.Reason), true
	case r.Err != nil:
		s.failed++
		return fmt.Sprintf("error %s: %v", r.File.Path, r.Err), true
	case r.Action == upload.ActionUploaded:
		s.uploaded++
		s.countKind(r.Kind)
		s.uploadedBytes += r.UploadedBytes
		return fmt.Sprintf("uploaded %s -> %s (%d bytes, %d messages)", r.File.Path, r.Destination(), r.UploadedBytes, r.MessageCount), false
	case r.Action == upload.ActionReset:
		s.reset++
		s.countKind(r.Kind)
		s.uploadedBytes += r.UploadedBytes
		return fmt.Sprintf("reset+uploaded %s -> %s (%d bytes)", r.File.Path, r.Destination(), r.UploadedBytes), false
	case r.Action == upload.ActionUpToDate:
		s.upToDate++
		s.countKind(r.Kind)
		return "", false
	}
	return "", false
}

// syncAll runs each file through run concurrently, bounded by concurrency, and
// folds every outcome on a single goroutine so the summary and the printed lines
// stay consistent without locking. Because files now complete out of order, the
// per-file lines interleave with no fixed ordering; printing them from the one
// folding goroutine keeps each line whole.
//
// It stops scheduling new files as soon as deadline is cancelled (a first Ctrl-C
// or an elapsed time limit), but lets files already in flight finish on the
// detached work context: those run with no cancellation, exactly as the sequential
// loop let the single in-flight file finish. Slot acquisition is itself
// cancellation-aware, so a deadline that fires while a launch waits for a free
// slot stops the loop and is reported, rather than letting a fresh file start or
// dropping that file silently. It returns the folded summary and whether it
// stopped early.
//
// The file-level cap is intentionally modest. Each file already fans its own body
// uploads out under the client's shared adaptive limiter and CPU-bounded encoder,
// so the file loop only needs enough parallelism to overlap the per-file announce
// and existence-check round-trips; a large cap would multiply against that
// per-file fan-out without bounding aggregate network or CPU any better.
func syncAll(work, deadline context.Context, files []discover.File, concurrency int, run func(context.Context, discover.File) outcome) (*summary, bool) {
	sum := newSummary()
	results := make(chan outcome, concurrency)

	// One goroutine owns the summary and stdout/stderr, so neither the counts nor a
	// printed line can interleave with another file's.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for o := range results {
			line, stderr := sum.fold(o)
			if line == "" {
				continue
			}
			if stderr {
				fmt.Fprintln(os.Stderr, line)
			} else {
				fmt.Println(line)
			}
		}
	}()

	// sem bounds how many files run at once. Acquiring a slot can block: a slot
	// frees only when an in-flight file finishes, which may be deep into a slow
	// upload. The deadline can fire during that wait, so acquisition has to watch
	// for it rather than block obliviously.
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	interrupted := false
	for _, f := range files {
		if deadline.Err() != nil {
			interrupted = true
			break
		}
		// Wait for a free slot, but stop the moment the deadline fires: cancellation
		// must neither start a new file nor leave one unaccounted for.
		select {
		case sem <- struct{}{}:
		case <-deadline.Done():
			interrupted = true
		}
		if interrupted {
			break
		}
		// A slot can free in the same instant the deadline fires, and the send above
		// may win that race; re-check and hand the slot back rather than start a file
		// after cancellation has begun.
		if deadline.Err() != nil {
			<-sem
			interrupted = true
			break
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			results <- run(work, f)
		}()
	}
	wg.Wait()
	close(results)
	<-done
	return sum, interrupted
}

func printSummary(total int, s *summary, dryRun bool) {
	if dryRun {
		fmt.Printf("\n%d file(s) discovered, %d skipped (dry run, nothing uploaded)\n", total, s.skipped)
	} else {
		fmt.Printf("\n%d file(s): %d uploaded, %d reset, %d up to date, %d skipped, %d failed (%d bytes sent)\n",
			total, s.uploaded, s.reset, s.upToDate, s.skipped, s.failed, s.uploadedBytes)
	}
	if s.standalone > 0 || s.orphaned > 0 {
		fmt.Printf("of which %d standalone, %d orphaned (backed up, no git remote)\n", s.standalone, s.orphaned)
	}
	if len(s.skipReasons) > 0 {
		keys := make([]string, 0, len(s.skipReasons))
		for k := range s.skipReasons {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fmt.Println("skips by reason:")
		for _, k := range keys {
			fmt.Printf("  %3d  %s\n", s.skipReasons[k], k)
		}
	}
}
