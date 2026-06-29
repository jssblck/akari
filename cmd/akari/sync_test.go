package main

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jssblck/akari/internal/client/discover"
	"github.com/jssblck/akari/internal/client/resolve"
	"github.com/jssblck/akari/internal/client/syncer"
	"github.com/jssblck/akari/internal/client/upload"
)

// TestSyncDeadlineCancelsAfterLimit verifies the time limit behaves like a
// self-inflicted graceful shutdown: the context the sync loop gates on cancels on
// its own once the limit elapses, with the deadline as the cause.
func TestSyncDeadlineCancelsAfterLimit(t *testing.T) {
	deadline, cancel := syncDeadline(context.Background(), 20*time.Millisecond)
	defer cancel()

	select {
	case <-deadline.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("deadline context was not cancelled after the limit elapsed")
	}
	if got := deadline.Err(); got != context.DeadlineExceeded {
		t.Fatalf("deadline.Err() = %v, want %v", got, context.DeadlineExceeded)
	}
}

// TestSyncDeadlineZeroMeansNoLimit guards the documented infinite case: a
// non-positive limit must leave the context live so the loop runs until the work
// is done rather than stopping itself.
func TestSyncDeadlineZeroMeansNoLimit(t *testing.T) {
	for _, limit := range []time.Duration{0, -time.Second} {
		deadline, cancel := syncDeadline(context.Background(), limit)
		if _, hasDeadline := deadline.Deadline(); hasDeadline {
			cancel()
			t.Fatalf("limit %v: context has a deadline, want none", limit)
		}
		select {
		case <-deadline.Done():
			cancel()
			t.Fatalf("limit %v: context cancelled itself, want live until cancel", limit)
		case <-time.After(50 * time.Millisecond):
		}
		cancel()
	}
}

// TestSyncDeadlinePropagatesParentCancel confirms a Ctrl-C on the parent shutdown
// context still stops the loop even when a finite time limit is in force, so the
// two stop conditions compose instead of one masking the other.
func TestSyncDeadlinePropagatesParentCancel(t *testing.T) {
	parent, parentCancel := context.WithCancel(context.Background())
	deadline, cancel := syncDeadline(parent, time.Hour)
	defer cancel()

	parentCancel()
	select {
	case <-deadline.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("deadline context did not cancel when parent was cancelled")
	}
}

// mixedOutcomes returns one of every outcome the fold path handles, so a test can
// assert the tally and exercise both the dry-run and real-sync branches. The
// session identities are drawn from women in computing history.
func mixedOutcomes() []outcome {
	file := func(name string) discover.File { return discover.File{Path: name} }
	upTo := func(name string, k resolve.Kind) outcome {
		return outcome{sync: &syncer.Result{File: file(name), Kind: k, Action: upload.ActionUpToDate}}
	}
	return []outcome{
		{sync: &syncer.Result{File: file("hopper"), Kind: resolve.KindRemote, ProjectKey: "github.com/x/y", Action: upload.ActionUploaded, UploadedBytes: 100, MessageCount: 3}},
		{sync: &syncer.Result{File: file("lovelace"), Kind: resolve.KindStandalone, Cwd: "/home/ada", Action: upload.ActionReset, UploadedBytes: 50}},
		upTo("winlock", resolve.KindRemote),
		upTo("johnson", resolve.KindOrphaned),
		{sync: &syncer.Result{File: file("clarke"), Skipped: true, Reason: "could not read header"}},
		{sync: &syncer.Result{File: file("easley"), Err: fmt.Errorf("connection refused")}},
	}
}

// foldInto folds every outcome in order and returns the resulting summary, the
// helper the count tests share.
func foldInto(outcomes []outcome) *summary {
	s := newSummary()
	for _, o := range outcomes {
		s.fold(o)
	}
	return s
}

// TestSummaryFoldCounts pins the tally for a representative mix of outcomes so the
// concurrent fold path is held to the same accounting the sequential loop did.
func TestSummaryFoldCounts(t *testing.T) {
	s := foldInto(mixedOutcomes())
	if s.uploaded != 1 || s.reset != 1 || s.upToDate != 2 || s.skipped != 1 || s.failed != 1 {
		t.Fatalf("action counts: uploaded=%d reset=%d upToDate=%d skipped=%d failed=%d", s.uploaded, s.reset, s.upToDate, s.skipped, s.failed)
	}
	if s.standalone != 1 || s.orphaned != 1 {
		t.Fatalf("kind counts: standalone=%d orphaned=%d", s.standalone, s.orphaned)
	}
	if s.uploadedBytes != 150 {
		t.Fatalf("uploadedBytes = %d, want 150", s.uploadedBytes)
	}
	if got := s.skipReasons["could not read header"]; got != 1 {
		t.Fatalf("skipReasons[could not read header] = %d, want 1", got)
	}
}

// TestSummaryFoldOrderIndependent is the property the concurrency relies on:
// because files complete out of order, folding the same outcomes in a different
// order must produce the identical summary.
func TestSummaryFoldOrderIndependent(t *testing.T) {
	outcomes := mixedOutcomes()
	forward := foldInto(outcomes)

	reversed := make([]outcome, len(outcomes))
	for i, o := range outcomes {
		reversed[len(outcomes)-1-i] = o
	}
	backward := foldInto(reversed)

	if !reflect.DeepEqual(forward, backward) {
		t.Fatalf("fold order changed the tally:\n forward=%+v\nbackward=%+v", forward, backward)
	}
}

// TestSyncAllCountsMatchSequential runs the concurrent driver over a mix of files
// and confirms the totals equal a straight fold of the same outcomes, i.e. running
// many files at once loses or double-counts nothing.
func TestSyncAllCountsMatchSequential(t *testing.T) {
	const n = 200
	files := make([]discover.File, n)
	for i := range files {
		files[i] = discover.File{Path: fmt.Sprintf("session-%03d", i)}
	}

	// A deterministic outcome per file, cycling through every action so the tally
	// has something of each kind to get right.
	outcomeFor := func(f discover.File, i int) outcome {
		switch i % 5 {
		case 0:
			return outcome{sync: &syncer.Result{File: f, Kind: resolve.KindRemote, Action: upload.ActionUploaded, UploadedBytes: 10, MessageCount: 1}}
		case 1:
			return outcome{sync: &syncer.Result{File: f, Kind: resolve.KindStandalone, Action: upload.ActionReset, UploadedBytes: 5}}
		case 2:
			return outcome{sync: &syncer.Result{File: f, Kind: resolve.KindRemote, Action: upload.ActionUpToDate}}
		case 3:
			return outcome{sync: &syncer.Result{File: f, Skipped: true, Reason: "no header"}}
		default:
			return outcome{sync: &syncer.Result{File: f, Err: fmt.Errorf("boom")}}
		}
	}

	want := newSummary()
	for i, f := range files {
		want.fold(outcomeFor(f, i))
	}

	idx := map[string]int{}
	for i, f := range files {
		idx[f.Path] = i
	}
	run := func(_ context.Context, f discover.File) outcome {
		return outcomeFor(f, idx[f.Path])
	}

	got, interrupted := syncAll(context.Background(), context.Background(), files, 16, run)
	if interrupted {
		t.Fatal("syncAll reported interrupted with a live deadline")
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("concurrent tally differs from sequential:\n got=%+v\nwant=%+v", got, want)
	}
	if total := got.uploaded + got.reset + got.upToDate + got.skipped + got.failed; total != n {
		t.Fatalf("accounted for %d files, want %d", total, n)
	}
}

// TestSyncAllStopsSchedulingAfterDeadline confirms the interruption contract holds
// under concurrency: once the deadline cancels, the driver stops starting new
// files (it does not run the whole list) yet still folds the ones it did start and
// reports interrupted.
func TestSyncAllStopsSchedulingAfterDeadline(t *testing.T) {
	const n = 100
	files := make([]discover.File, n)
	for i := range files {
		files[i] = discover.File{Path: fmt.Sprintf("session-%03d", i)}
	}

	deadlineCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var started int32
	run := func(_ context.Context, f discover.File) outcome {
		// Cancel partway through to mimic a Ctrl-C or an elapsed time limit landing
		// mid-run. Concurrency of 1 keeps the started count from racing far past it.
		if atomic.AddInt32(&started, 1) == 5 {
			cancel()
		}
		return outcome{sync: &syncer.Result{File: f, Action: upload.ActionUpToDate}}
	}

	sum, interrupted := syncAll(context.Background(), deadlineCtx, files, 1, run)
	if !interrupted {
		t.Fatal("expected interrupted = true after the deadline fired")
	}
	if got := atomic.LoadInt32(&started); got >= n {
		t.Fatalf("started %d files, want fewer than %d (should have stopped scheduling)", got, n)
	}
	// Every started file is still folded, so the tally is never silently dropped.
	if sum.upToDate != int(atomic.LoadInt32(&started)) {
		t.Fatalf("folded %d up-to-date, but started %d", sum.upToDate, started)
	}
}

// TestSyncAllDeadlineDuringSlotWaitStartsNoNewFile pins the interruption edge that
// concurrency exposes: with the slots saturated, a launch blocks inside the
// scheduler waiting for one to free, and that wait can outlast the deadline. When
// the deadline fires during the wait, no fresh file may start; only the files
// already running finish. This is the case concurrency 1 cannot reach, since
// nothing blocks on a slot there.
func TestSyncAllDeadlineDuringSlotWaitStartsNoNewFile(t *testing.T) {
	const concurrency = 2
	const n = 10
	files := make([]discover.File, n)
	for i := range files {
		files[i] = discover.File{Path: fmt.Sprintf("session-%02d", i)}
	}

	deadlineCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var started int32
	firstWaveFull := make(chan struct{}, concurrency) // signals each held slot
	release := make(chan struct{})                    // frees the first wave

	run := func(_ context.Context, f discover.File) outcome {
		atomic.AddInt32(&started, 1)
		// The first wave holds its slots until released, so every later launch is
		// blocked in the scheduler waiting for a slot when the deadline fires.
		firstWaveFull <- struct{}{}
		<-release
		return outcome{sync: &syncer.Result{File: f, Action: upload.ActionUpToDate}}
	}

	go func() {
		// Once both slots are occupied, cancel while the scheduler is blocked trying
		// to launch the next file, then free the wave so slots reopen.
		for i := 0; i < concurrency; i++ {
			<-firstWaveFull
		}
		cancel()
		close(release)
	}()

	sum, interrupted := syncAll(context.Background(), deadlineCtx, files, concurrency, run)
	if !interrupted {
		t.Fatal("expected interrupted = true after the deadline fired")
	}
	if got := atomic.LoadInt32(&started); got != concurrency {
		t.Fatalf("started %d files, want exactly %d: a file began after cancellation", got, concurrency)
	}
	if sum.upToDate != concurrency {
		t.Fatalf("folded %d up-to-date, want %d", sum.upToDate, concurrency)
	}
}

// TestSyncAllCancellingLastFileReportsInterrupted nails the subtle case where the
// deadline fires while the final file is waiting for a slot. The file must not run
// (no new work after cancellation) and the run must still report interrupted, so
// the file is neither processed nor silently dropped from the accounting.
func TestSyncAllCancellingLastFileReportsInterrupted(t *testing.T) {
	files := []discover.File{{Path: "hopper"}, {Path: "lovelace"}}

	deadlineCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var started int32
	held := make(chan struct{})
	release := make(chan struct{})
	run := func(_ context.Context, f discover.File) outcome {
		atomic.AddInt32(&started, 1)
		held <- struct{}{} // the lone slot is now taken
		<-release
		return outcome{sync: &syncer.Result{File: f, Action: upload.ActionUpToDate}}
	}

	go func() {
		<-held   // the first file holds the only slot
		cancel() // deadline fires while the last file waits for a slot
		close(release)
	}()

	sum, interrupted := syncAll(context.Background(), deadlineCtx, files, 1, run)
	if !interrupted {
		t.Fatal("cancelling while the last file waits for a slot must report interrupted")
	}
	if got := atomic.LoadInt32(&started); got != 1 {
		t.Fatalf("started %d files, want 1: the last file must not run after cancellation", got)
	}
	if sum.upToDate != 1 {
		t.Fatalf("folded %d up-to-date, want 1", sum.upToDate)
	}
}

// TestSyncAllRoutesDryRunOutcomes exercises the fold dispatcher through the
// concurrent driver: a regression in routing outcome.resolve to the dry-run fold
// path would be caught here, not just in the direct foldResolve test.
func TestSyncAllRoutesDryRunOutcomes(t *testing.T) {
	files := []discover.File{{Path: "lovelace"}, {Path: "clarke"}}
	run := func(_ context.Context, f discover.File) outcome {
		if f.Path == "clarke" {
			return outcome{resolve: &resolve.Result{File: f, Skipped: true, Reason: "could not read header"}}
		}
		return outcome{resolve: &resolve.Result{File: f, Kind: resolve.KindStandalone, Header: resolve.Header{Cwd: "/home/ada"}}}
	}

	sum, interrupted := syncAll(context.Background(), context.Background(), files, 4, run)
	if interrupted {
		t.Fatal("syncAll reported interrupted with a live deadline")
	}
	if sum.skipped != 1 || sum.standalone != 1 {
		t.Fatalf("dry-run routing: skipped=%d standalone=%d, want 1 and 1", sum.skipped, sum.standalone)
	}
	if sum.uploaded != 0 {
		t.Fatalf("a dry run uploads nothing, but uploaded=%d", sum.uploaded)
	}
}

// TestFoldResolve covers the dry-run fold branches end to end: a skip tallies its
// reason and goes to stderr, and each resolvable kind reports the destination a
// real run would have uploaded to, on stdout.
func TestFoldResolve(t *testing.T) {
	file := func(name string) discover.File { return discover.File{Path: name} }

	t.Run("skipped", func(t *testing.T) {
		s := newSummary()
		line, stderr := s.foldResolve(resolve.Result{File: file("clarke"), Skipped: true, Reason: "could not read header"})
		if !stderr {
			t.Fatal("a skip should print to stderr")
		}
		if line != "skip clarke: could not read header" {
			t.Fatalf("line = %q", line)
		}
		if s.skipped != 1 || s.skipReasons["could not read header"] != 1 {
			t.Fatalf("skipped=%d reasons=%v", s.skipped, s.skipReasons)
		}
	})

	t.Run("standalone", func(t *testing.T) {
		s := newSummary()
		line, stderr := s.foldResolve(resolve.Result{File: file("lovelace"), Kind: resolve.KindStandalone, Header: resolve.Header{Cwd: "/home/ada"}})
		if stderr {
			t.Fatal("a would-upload line belongs on stdout")
		}
		if line != "would upload lovelace -> standalone:/home/ada" {
			t.Fatalf("line = %q", line)
		}
		if s.standalone != 1 {
			t.Fatalf("standalone = %d, want 1", s.standalone)
		}
	})

	t.Run("orphaned", func(t *testing.T) {
		s := newSummary()
		line, _ := s.foldResolve(resolve.Result{File: file("johnson"), Kind: resolve.KindOrphaned})
		if line != "would upload johnson -> orphaned" {
			t.Fatalf("line = %q", line)
		}
		if s.orphaned != 1 {
			t.Fatalf("orphaned = %d, want 1", s.orphaned)
		}
	})

	t.Run("remote", func(t *testing.T) {
		s := newSummary()
		line, _ := s.foldResolve(resolve.Result{File: file("winlock"), Kind: resolve.KindRemote, ProjectKey: "github.com/x/y"})
		if line != "would upload winlock -> github.com/x/y" {
			t.Fatalf("line = %q", line)
		}
		if s.standalone != 0 || s.orphaned != 0 {
			t.Fatalf("a remote dry run should not count standalone/orphaned: standalone=%d orphaned=%d", s.standalone, s.orphaned)
		}
	})
}

// TestRunSyncRejectsBadConcurrency confirms the flag is validated before any
// config or discovery work, so a bad value fails fast with a clear message.
func TestRunSyncRejectsBadConcurrency(t *testing.T) {
	for _, v := range []string{"0", "-1"} {
		err := runSync(context.Background(), []string{"--concurrency", v})
		if err == nil {
			t.Fatalf("concurrency %s: expected an error", v)
		}
		if !strings.Contains(err.Error(), "concurrency") {
			t.Fatalf("concurrency %s: error = %v, want it to mention concurrency", v, err)
		}
	}
}
