package resolve

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jssblck/akari/internal/client/discover"
	"golang.org/x/sync/singleflight"
)

func TestRunGitRecoversFromRepositoryLockContention(t *testing.T) {
	var calls atomic.Int32
	r := NewWith(func(_ context.Context, _ string, _ ...string) (string, error) {
		if calls.Add(1) == 1 {
			return "", fmt.Errorf("error: could not lock config file .git/config: File exists")
		}
		return "git@example.com:owner/repo.git", nil
	}, nil)
	r.retryDelay = func(int) time.Duration { return 0 }

	out, err := r.runGit(context.Background(), t.TempDir(), "remote", "get-url", "--all", "origin")
	if err != nil {
		t.Fatal(err)
	}
	if out != "git@example.com:owner/repo.git" {
		t.Fatalf("output = %q", out)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("Git calls = %d, want 2", got)
	}
}

func TestRunGitRecoversFromOutputReadFailure(t *testing.T) {
	var calls atomic.Int32
	r := NewWith(func(_ context.Context, _ string, _ ...string) (string, error) {
		if calls.Add(1) == 1 {
			return "", io.ErrUnexpectedEOF
		}
		return "true", nil
	}, nil)
	r.retryDelay = func(int) time.Duration { return 0 }

	if _, err := r.runGit(context.Background(), t.TempDir(), "rev-parse", "--is-inside-work-tree"); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("Git calls = %d, want 2", got)
	}
}

func TestRunGitBoundsProcessFailureAttempts(t *testing.T) {
	var calls atomic.Int32
	r := NewWith(func(_ context.Context, _ string, _ ...string) (string, error) {
		calls.Add(1)
		return "", exec.ErrNotFound
	}, nil)
	r.retryDelay = func(int) time.Duration { return 0 }

	_, err := r.runGit(context.Background(), t.TempDir(), "rev-parse", "--git-path", "config")
	if err == nil {
		t.Fatal("missing Git process unexpectedly succeeded")
	}
	if got := calls.Load(); got != gitMaxAttempts {
		t.Fatalf("Git calls = %d, want %d", got, gitMaxAttempts)
	}
	if got := err.Error(); strings.Count(got, "failed after") != 1 || !strings.Contains(got, "executable file not found") {
		t.Fatalf("final error = %q, want one attempt summary and the process cause", got)
	}
}

func TestRunGitCancellationStopsBackoff(t *testing.T) {
	firstAttempt := make(chan struct{})
	var calls atomic.Int32
	r := NewWith(func(_ context.Context, _ string, _ ...string) (string, error) {
		if calls.Add(1) == 1 {
			close(firstAttempt)
		}
		return "", errors.New("temporary process failure")
	}, nil)
	r.retryDelay = func(int) time.Duration { return time.Hour }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := r.runGit(ctx, t.TempDir(), "remote", "get-url", "--all", "origin")
		done <- err
	}()
	<-firstAttempt
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context cancellation", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("retry backoff ignored cancellation")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("Git calls after cancellation = %d, want 1", got)
	}
}

func TestRunGitDoesNotRetryDefinitiveResults(t *testing.T) {
	tests := []struct {
		name string
		args []string
		err  error
		want definitiveGitResult
	}{
		{
			name: "invalid repository",
			args: []string{"rev-parse", "--is-inside-work-tree"},
			err:  errors.New("fatal: not a git repository (or any parent up to mount point)"),
			want: gitResultNotRepository,
		},
		{
			name: "missing origin",
			args: []string{"remote", "get-url", "--all", "origin"},
			err:  errors.New("error: No such remote 'origin'"),
			want: gitResultNoRemote,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			r := NewWith(func(_ context.Context, _ string, _ ...string) (string, error) {
				calls.Add(1)
				return "", tt.err
			}, nil)
			r.retryDelay = func(int) time.Duration {
				t.Fatal("definitive result entered retry backoff")
				return 0
			}

			_, err := r.runGit(context.Background(), t.TempDir(), tt.args...)
			if err == nil || definitiveGitFailure(tt.args, err) != tt.want {
				t.Fatalf("error = %v, classification = %v", err, definitiveGitFailure(tt.args, err))
			}
			if got := calls.Load(); got != 1 {
				t.Fatalf("Git calls = %d, want 1", got)
			}
		})
	}
}

func TestGitRetryDelayIsTightlyBounded(t *testing.T) {
	for attempt, bounds := range map[int][2]time.Duration{
		1: {20 * time.Millisecond, 30 * time.Millisecond},
		2: {40 * time.Millisecond, 60 * time.Millisecond},
	} {
		for range 100 {
			delay := defaultGitRetryDelay(attempt)
			if delay < bounds[0] || delay > bounds[1] {
				t.Fatalf("attempt %d delay = %v, want within [%v, %v]", attempt, delay, bounds[0], bounds[1])
			}
		}
	}
}

func TestConcurrentProjectLookupsShareTransientRecovery(t *testing.T) {
	cwd := t.TempDir()
	configPath := filepath.Join(cwd, "git-config")
	writeFile(t, cwd, "git-config", "config")

	remoteStarted := make(chan struct{})
	releaseRemote := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(releaseRemote) }) })
	var remoteCalls atomic.Int32
	git := func(ctx context.Context, _ string, args ...string) (string, error) {
		switch {
		case hasArg(args, "--is-inside-work-tree"):
			return "true", nil
		case hasArg(args, "--git-path"):
			return configPath, nil
		case args[0] == "remote":
			call := remoteCalls.Add(1)
			if call == 1 {
				close(remoteStarted)
				select {
				case <-releaseRemote:
					return "", errors.New("error: could not lock config file .git/config: File exists")
				case <-ctx.Done():
					return "", ctx.Err()
				}
			}
			return "git@example.com:owner/recovered.git", nil
		default:
			return "", fmt.Errorf("unexpected Git arguments: %v", args)
		}
	}
	r := NewWith(git, nil)
	r.retryDelay = func(int) time.Duration { return 0 }

	const callers = 32
	results := make([]<-chan singleflight.Result, 0, callers)
	results = append(results, r.lookupProject(context.Background(), cwd))
	select {
	case <-remoteStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("shared lookup did not reach origin")
	}
	for range callers - 1 {
		results = append(results, r.lookupProject(context.Background(), cwd))
	}
	releaseOnce.Do(func() { close(releaseRemote) })

	for i, result := range results {
		select {
		case completed := <-result:
			if completed.Err != nil {
				t.Fatalf("caller %d: %v", i, completed.Err)
			}
			if !completed.Shared {
				t.Fatalf("caller %d did not share the singleflight lookup", i)
			}
			resolved, ok := completed.Val.(projectResult)
			if !ok || resolved.key != "example.com/owner/recovered" {
				t.Fatalf("caller %d result = %#v", i, completed.Val)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("caller %d did not receive the shared result", i)
		}
	}
	if got := remoteCalls.Load(); got != 2 {
		t.Fatalf("origin calls = %d, want 2 shared attempts", got)
	}
}

func TestGitCommandEnvPinsMessageLocale(t *testing.T) {
	t.Setenv("LANG", "fr_FR.UTF-8")
	t.Setenv("LC_ALL", "fr_FR.UTF-8")
	t.Setenv("LANGUAGE", "fr_FR")

	env := gitCommandEnv()

	want := map[string]string{"LC_ALL": "C", "LANGUAGE": "C", "LANG": "C"}
	got := make(map[string]string, len(want))
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if _, tracked := want[key]; tracked {
			got[key] = value
		}
	}
	for key, value := range want {
		if got[key] != value {
			t.Fatalf("gitCommandEnv()[%s] = %q, want %q (definitiveGitFailure depends on untranslated Git output)", key, got[key], value)
		}
	}
}

func TestResolveReportsExhaustedGitFailure(t *testing.T) {
	cwd := t.TempDir()
	session := writeFile(t, cwd, "session.jsonl", claudeLine(cwd))
	r := NewWith(func(_ context.Context, _ string, args ...string) (string, error) {
		if hasArg(args, "--is-inside-work-tree") {
			return "", errors.New("temporary Git process failure")
		}
		return "", fmt.Errorf("unexpected Git arguments: %v", args)
	}, nil)
	r.retryDelay = func(int) time.Duration { return 0 }

	res := r.Resolve(context.Background(), discover.File{Agent: "claude", Path: session})
	if res.Err == nil {
		t.Fatal("transient exhaustion was classified as a successful result")
	}
	if res.Kind != "" || res.Reason != "" || res.ProjectKey != "" {
		t.Fatalf("failed result carried classification: %+v", res)
	}
}
