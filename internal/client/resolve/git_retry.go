package resolve

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	gitMaxAttempts   = 3
	gitRetryBase     = 20 * time.Millisecond
	gitRetryMaxDelay = 50 * time.Millisecond
)

// gitCommandError preserves Git's stderr because exit status alone cannot
// distinguish a definitive repository state from short-lived lock or process
// failures. The resolver uses that distinction to avoid retrying known absences
// while keeping operational failures out of its classification cache.
type gitCommandError struct {
	args   []string
	stderr string
	err    error
}

func (e *gitCommandError) Error() string {
	detail := strings.TrimSpace(e.stderr)
	if detail == "" {
		return fmt.Sprintf("git %s: %v", strings.Join(e.args, " "), e.err)
	}
	return fmt.Sprintf("git %s: %v: %s", strings.Join(e.args, " "), e.err, detail)
}

func (e *gitCommandError) Unwrap() error { return e.err }

type definitiveGitResult uint8

const (
	gitResultUnknown definitiveGitResult = iota
	gitResultNotRepository
	gitResultNoRemote
)

// runGit retries operational failures within the lookup's existing five-second
// deadline. Three attempts add at most 90ms of backoff, including jitter, so a
// transient lock can clear without letting Git resolution dominate a sync.
func (r *Resolver) runGit(ctx context.Context, dir string, args ...string) (string, error) {
	var lastErr error
	for attempt := 1; attempt <= gitMaxAttempts; attempt++ {
		out, err := r.git(ctx, dir, args...)
		if err == nil {
			return out, nil
		}
		lastErr = err
		if definitiveGitFailure(args, err) != gitResultUnknown {
			return "", err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", ctxErr
		}
		if attempt == gitMaxAttempts {
			break
		}
		if err := waitForRetry(ctx, r.retryDelay(attempt)); err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("git %s failed after %d attempts: %w", strings.Join(args, " "), gitMaxAttempts, lastErr)
}

func defaultGitRetryDelay(attempt int) time.Duration {
	delay := gitRetryBase << (attempt - 1)
	if delay > gitRetryMaxDelay {
		delay = gitRetryMaxDelay
	}
	// A half-delay jitter keeps files resolving different repositories from
	// repeatedly colliding on process or filesystem locks at the same instant.
	return delay + time.Duration(rand.Int64N(int64(delay/2)+1))
}

func waitForRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func definitiveGitFailure(args []string, err error) definitiveGitResult {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return gitResultUnknown
	}
	message := strings.ToLower(err.Error())
	if containsGitArg(args, "--is-inside-work-tree") &&
		(strings.Contains(message, "not a git repository") ||
			strings.Contains(message, "not a git repo") ||
			strings.Contains(message, "must be run in a work tree")) {
		return gitResultNotRepository
	}
	if len(args) > 0 && args[0] == "remote" &&
		(strings.Contains(message, "no such remote") ||
			strings.Contains(message, "no remote configured")) {
		return gitResultNoRemote
	}
	return gitResultUnknown
}

func containsGitArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

// systemGit keeps stderr attached to command failures so runGit can decide
// whether an exit represents a stable repository state or a retryable failure.
//
// It pins the subprocess to the C locale because definitiveGitFailure matches
// known-stable outcomes against the untranslated English text Git prints on
// exit. Without a pinned locale, a machine with LANG or LC_ALL set to a
// translated locale makes Git emit messages in that language, so an ordinary
// "not a git repository" never matches and gets retried three times before
// surfacing as an uncached transient failure instead of being classified.
func systemGit(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = gitCommandEnv()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", &gitCommandError{args: append([]string(nil), args...), stderr: stderr.String(), err: err}
	}
	return strings.TrimSpace(string(out)), nil
}

// gitCommandEnv returns the environment for the Git subprocess with its
// message locale pinned to C. Entries appended after os.Environ() win over
// any earlier duplicate keys (exec.Cmd keeps only the last value for each
// key), so this reliably overrides an inherited LANG, LC_ALL, or LANGUAGE
// regardless of the user's shell locale.
func gitCommandEnv() []string {
	return append(os.Environ(), "LC_ALL=C", "LANGUAGE=C", "LANG=C")
}
