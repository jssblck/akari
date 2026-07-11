package resolve

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sync/singleflight"
)

func TestResolverCoalescesConcurrentProjectLookups(t *testing.T) {
	cwd := t.TempDir()
	configPath := filepath.Join(cwd, "git-config")
	if err := os.WriteFile(configPath, []byte("config"), 0o600); err != nil {
		t.Fatal(err)
	}

	lookupStarted := make(chan struct{})
	releaseLookup := make(chan struct{})
	var startedOnce sync.Once
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(releaseLookup) }) })
	var insideCalls atomic.Int32
	var remoteCalls atomic.Int32
	git := func(ctx context.Context, _ string, args ...string) (string, error) {
		switch {
		case hasArg(args, "--is-inside-work-tree"):
			insideCalls.Add(1)
			startedOnce.Do(func() { close(lookupStarted) })
			select {
			case <-releaseLookup:
				return "true", nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		case hasArg(args, "--git-path"):
			return configPath, nil
		case args[0] == "remote":
			remoteCalls.Add(1)
			return "git@example.com:owner/repo.git", nil
		default:
			return "", fmt.Errorf("unexpected git arguments: %v", args)
		}
	}
	r := NewWith(git, nil)

	const callers = 32
	results := make([]<-chan singleflight.Result, 0, callers)
	results = append(results, r.lookupProject(context.Background(), cwd))
	select {
	case <-lookupStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("shared Git lookup did not start")
	}
	for range callers - 1 {
		results = append(results, r.lookupProject(context.Background(), cwd))
	}
	releaseOnce.Do(func() { close(releaseLookup) })

	for i, result := range results {
		select {
		case completed := <-result:
			if completed.Err != nil {
				t.Fatalf("caller %d: lookup error: %v", i, completed.Err)
			}
			resolved, ok := completed.Val.(projectResult)
			if !ok || resolved.key != "example.com/owner/repo" || resolved.reason != "" {
				t.Fatalf("caller %d: result = %#v", i, completed.Val)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("caller %d: shared lookup did not finish", i)
		}
	}
	if got := insideCalls.Load(); got != 1 {
		t.Fatalf("inside-work-tree calls = %d, want 1", got)
	}
	if got := remoteCalls.Load(); got != 1 {
		t.Fatalf("remote calls = %d, want 1", got)
	}
}

func TestResolverCallerCancellationDoesNotCancelSharedLookup(t *testing.T) {
	cwd := t.TempDir()
	configPath := filepath.Join(cwd, "git-config")
	if err := os.WriteFile(configPath, []byte("config"), 0o600); err != nil {
		t.Fatal(err)
	}

	lookupStarted := make(chan struct{})
	inspectContext := make(chan struct{})
	releaseLookup := make(chan struct{})
	contextStatus := make(chan error, 1)
	var startedOnce sync.Once
	var inspectOnce sync.Once
	var releaseOnce sync.Once
	t.Cleanup(func() {
		inspectOnce.Do(func() { close(inspectContext) })
		releaseOnce.Do(func() { close(releaseLookup) })
	})
	var remoteCalls atomic.Int32
	git := func(ctx context.Context, _ string, args ...string) (string, error) {
		switch {
		case hasArg(args, "--is-inside-work-tree"):
			startedOnce.Do(func() { close(lookupStarted) })
			<-inspectContext
			contextStatus <- ctx.Err()
			if err := ctx.Err(); err != nil {
				return "", err
			}
			select {
			case <-releaseLookup:
				return "true", nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		case hasArg(args, "--git-path"):
			return configPath, nil
		case args[0] == "remote":
			remoteCalls.Add(1)
			return "git@example.com:owner/repo.git", nil
		default:
			return "", fmt.Errorf("unexpected git arguments: %v", args)
		}
	}
	r := NewWith(git, nil)

	callerCtx, cancelCaller := context.WithCancel(context.Background())
	callerDone := make(chan error, 1)
	go func() {
		_, _, _, err := r.project(callerCtx, cwd)
		callerDone <- err
	}()
	select {
	case <-lookupStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("shared Git lookup did not start")
	}

	cancelCaller()
	select {
	case err := <-callerDone:
		if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "project lookup canceled") {
			t.Fatalf("canceled caller error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceled caller waited for the shared lookup")
	}

	inspectOnce.Do(func() { close(inspectContext) })
	select {
	case err := <-contextStatus:
		if err != nil {
			t.Fatalf("caller cancellation reached shared lookup context: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shared lookup did not inspect its context")
	}

	waiter := r.lookupProject(context.Background(), cwd)
	releaseOnce.Do(func() { close(releaseLookup) })
	select {
	case completed := <-waiter:
		if completed.Err != nil {
			t.Fatal(completed.Err)
		}
		resolved, ok := completed.Val.(projectResult)
		if !ok || resolved.key != "example.com/owner/repo" || resolved.reason != "" {
			t.Fatalf("shared lookup result = %#v", completed.Val)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shared lookup did not finish after release")
	}

	key, _, reason, err := r.project(context.Background(), cwd)
	if err != nil {
		t.Fatal(err)
	}
	if key != "example.com/owner/repo" || reason != "" {
		t.Fatalf("cached result = key %q, reason %q", key, reason)
	}
	if got := remoteCalls.Load(); got != 1 {
		t.Fatalf("remote calls = %d, want 1", got)
	}
}

func TestResolverSlowConfigStatDoesNotBlockOtherRepositories(t *testing.T) {
	dirs := []string{t.TempDir(), t.TempDir()}
	configs := map[string]string{}
	for _, dir := range dirs {
		configs[dir] = filepath.Join(dir, "git-config")
		if err := os.WriteFile(configs[dir], []byte("config"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	git := func(_ context.Context, dir string, args ...string) (string, error) {
		switch {
		case hasArg(args, "--is-inside-work-tree"):
			return "true", nil
		case hasArg(args, "--git-path"):
			return configs[dir], nil
		case args[0] == "remote":
			return "git@example.com:owner/" + filepath.Base(dir) + ".git", nil
		default:
			return "", fmt.Errorf("unexpected git arguments: %v", args)
		}
	}
	r := NewWith(git, nil)
	for _, dir := range dirs {
		if key, _, reason, err := r.project(context.Background(), dir); key == "" || reason != "" || err != nil {
			t.Fatalf("seed %s = key %q, reason %q", dir, key, reason)
		}
	}

	statStarted := make(chan struct{})
	releaseStat := make(chan struct{})
	var startedOnce sync.Once
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(releaseStat) }) })
	r.stat = func(path string) (os.FileInfo, error) {
		if path == configs[dirs[0]] {
			startedOnce.Do(func() { close(statStarted) })
			<-releaseStat
		}
		return os.Stat(path)
	}

	firstDone := make(chan projectResult, 1)
	go func() {
		key, root, reason, _ := r.project(context.Background(), dirs[0])
		firstDone <- projectResult{key: key, root: root, reason: reason}
	}()
	select {
	case <-statStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("first repository did not enter its config stat")
	}

	secondDone := make(chan projectResult, 1)
	go func() {
		key, root, reason, _ := r.project(context.Background(), dirs[1])
		secondDone <- projectResult{key: key, root: root, reason: reason}
	}()
	select {
	case result := <-secondDone:
		if result.key == "" || result.reason != "" {
			t.Fatalf("second repository result = %+v", result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("unrelated repository blocked behind a slow config stat")
	}

	releaseOnce.Do(func() { close(releaseStat) })
	select {
	case result := <-firstDone:
		if result.key == "" || result.reason != "" {
			t.Fatalf("first repository result = %+v", result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first repository did not finish after releasing stat")
	}
}

func TestResolverDelayedStatCannotOverwriteNewerCacheEntry(t *testing.T) {
	cwd := t.TempDir()
	configPath := filepath.Join(cwd, "git-config")
	if err := os.WriteFile(configPath, []byte("config"), 0o600); err != nil {
		t.Fatal(err)
	}
	git := func(_ context.Context, _ string, args ...string) (string, error) {
		switch {
		case hasArg(args, "--is-inside-work-tree"):
			return "true", nil
		case hasArg(args, "--git-path"):
			return configPath, nil
		case args[0] == "remote":
			return "git@example.com:owner/old.git", nil
		default:
			return "", fmt.Errorf("unexpected git arguments: %v", args)
		}
	}
	r := NewWith(git, nil)
	if key, _, reason, err := r.project(context.Background(), cwd); key != "example.com/owner/old" || reason != "" || err != nil {
		t.Fatalf("seed = key %q, reason %q", key, reason)
	}
	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}

	statStarted := make(chan struct{})
	releaseStat := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(releaseStat) }) })
	var statCalls atomic.Int32
	r.stat = func(path string) (os.FileInfo, error) {
		if statCalls.Add(1) == 1 {
			close(statStarted)
			<-releaseStat
		}
		return os.Stat(path)
	}

	resultDone := make(chan projectResult, 1)
	go func() {
		result, _ := r.cachedProject(cwd, r.now())
		resultDone <- result
	}()
	select {
	case <-statStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("cached lookup did not enter config stat")
	}

	newer := projectResult{key: "example.com/owner/new"}
	r.storeProject(cwd, newer, configFingerprint{path: configPath, info: info}, r.now().Add(time.Second))
	releaseOnce.Do(func() { close(releaseStat) })
	select {
	case result := <-resultDone:
		if result != newer {
			t.Fatalf("delayed stat returned %+v, want %+v", result, newer)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cached lookup did not finish after replacing its entry")
	}

	r.mu.Lock()
	stored := r.cache[cwd]
	r.mu.Unlock()
	if stored.key != newer.key {
		t.Fatalf("delayed stat overwrote cache key with %q", stored.key)
	}
}
