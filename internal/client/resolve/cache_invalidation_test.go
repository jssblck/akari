package resolve

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestResolverCacheEvictsLeastRecentlyUsedDirectory(t *testing.T) {
	dirs := make([]string, 3)
	configs := make(map[string]string, len(dirs))
	for i := range dirs {
		dirs[i] = t.TempDir()
		configs[dirs[i]] = filepath.Join(dirs[i], "git-config")
		if err := os.WriteFile(configs[dirs[i]], []byte("config"), 0o600); err != nil {
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
			return fmt.Sprintf("git@example.com:owner/repo-%s.git", filepath.Base(dir)), nil
		default:
			return "", fmt.Errorf("unexpected git arguments: %v", args)
		}
	}
	r := NewWith(git, nil)
	r.cacheLimit = 2

	for _, dir := range dirs[:2] {
		if key, _, reason := r.project(context.Background(), dir); key == "" || reason != "" {
			t.Fatalf("resolve %s = key %q, reason %q", dir, key, reason)
		}
	}
	if key, _, _ := r.project(context.Background(), dirs[0]); key == "" {
		t.Fatal("cached first directory lost its key")
	}
	if key, _, _ := r.project(context.Background(), dirs[2]); key == "" {
		t.Fatal("third directory did not resolve")
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.cache) != 2 {
		t.Fatalf("cache contains %d entries, want 2", len(r.cache))
	}
	if _, ok := r.cache[dirs[1]]; ok {
		t.Fatalf("least recently used directory %q was not evicted", dirs[1])
	}
	if _, ok := r.cache[dirs[0]]; !ok {
		t.Fatalf("recently used directory %q was evicted", dirs[0])
	}
}

func TestResolverCacheInvalidatesWhenGitConfigChanges(t *testing.T) {
	cwd := t.TempDir()
	configPath := filepath.Join(cwd, "git-config")
	if err := os.WriteFile(configPath, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	remote := "git@example.com:owner/first.git"
	calls := 0
	git := func(_ context.Context, _ string, args ...string) (string, error) {
		calls++
		switch {
		case hasArg(args, "--is-inside-work-tree"):
			return "true", nil
		case hasArg(args, "--git-path"):
			return configPath, nil
		case args[0] == "remote":
			return remote, nil
		default:
			return "", fmt.Errorf("unexpected git arguments: %v", args)
		}
	}
	r := NewWith(git, nil)

	if key, _, _ := r.project(context.Background(), cwd); key != "example.com/owner/first" {
		t.Fatalf("first key = %q", key)
	}
	if key, _, _ := r.project(context.Background(), cwd); key != "example.com/owner/first" || calls != 3 {
		t.Fatalf("unchanged config key = %q after %d git calls, want cached result", key, calls)
	}
	remote = "git@example.com:owner/second.git"
	if err := os.WriteFile(configPath, []byte("second-config"), 0o600); err != nil {
		t.Fatal(err)
	}
	if key, _, _ := r.project(context.Background(), cwd); key != "example.com/owner/second" {
		t.Fatalf("changed config key = %q, want refreshed remote", key)
	}
	if calls != 6 {
		t.Fatalf("git calls after config change = %d, want 6", calls)
	}
}

func TestResolverCacheExpiresSuccessfulFingerprint(t *testing.T) {
	cwd := t.TempDir()
	configPath := filepath.Join(cwd, "git-config")
	if err := os.WriteFile(configPath, []byte("config"), 0o600); err != nil {
		t.Fatal(err)
	}
	remote := "git@example.com:owner/first.git"
	remoteCalls := 0
	git := func(_ context.Context, _ string, args ...string) (string, error) {
		switch {
		case hasArg(args, "--is-inside-work-tree"):
			return "true", nil
		case hasArg(args, "--git-path"):
			return configPath, nil
		case args[0] == "remote":
			remoteCalls++
			return remote, nil
		default:
			return "", fmt.Errorf("unexpected git arguments: %v", args)
		}
	}
	r := NewWith(git, nil)
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }
	r.cacheFallbackTTL = time.Minute

	if key, _, _ := r.project(context.Background(), cwd); key != "example.com/owner/first" {
		t.Fatalf("first key = %q", key)
	}
	remote = "git@example.com:owner/second.git"
	now = now.Add(30 * time.Second)
	if key, _, _ := r.project(context.Background(), cwd); key != "example.com/owner/first" {
		t.Fatalf("unexpired key = %q, want cached first remote", key)
	}
	if remoteCalls != 1 {
		t.Fatalf("unexpired entry made %d remote calls, want 1", remoteCalls)
	}

	now = now.Add(31 * time.Second)
	if key, _, _ := r.project(context.Background(), cwd); key != "example.com/owner/second" {
		t.Fatalf("expired key = %q, want refreshed remote", key)
	}
	if remoteCalls != 2 {
		t.Fatalf("expired entry made %d remote calls, want 2", remoteCalls)
	}
}

func TestResolverCacheRetriesTransientRemoteFailureAfterTTL(t *testing.T) {
	cwd := t.TempDir()
	configPath := filepath.Join(cwd, "git-config")
	if err := os.WriteFile(configPath, []byte("config"), 0o600); err != nil {
		t.Fatal(err)
	}
	remoteHealthy := false
	remoteCalls := 0
	git := func(_ context.Context, _ string, args ...string) (string, error) {
		switch {
		case hasArg(args, "--is-inside-work-tree"):
			return "true", nil
		case hasArg(args, "--git-path"):
			return configPath, nil
		case hasArg(args, "--git-common-dir"):
			return filepath.Join(cwd, ".git"), nil
		case args[0] == "remote":
			remoteCalls++
			if !remoteHealthy {
				return "", fmt.Errorf("temporary git failure")
			}
			return "git@example.com:owner/recovered.git", nil
		default:
			return "", fmt.Errorf("unexpected git arguments: %v", args)
		}
	}
	r := NewWith(git, nil)
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }
	r.cacheFallbackTTL = time.Minute

	if key, _, reason := r.project(context.Background(), cwd); key != "" || reason == "" {
		t.Fatalf("failed remote resolve = key %q, reason %q", key, reason)
	}
	remoteHealthy = true
	now = now.Add(30 * time.Second)
	if key, _, reason := r.project(context.Background(), cwd); key != "" || reason == "" {
		t.Fatalf("unexpired failed resolve = key %q, reason %q", key, reason)
	}
	if remoteCalls != 1 {
		t.Fatalf("unexpired failure made %d remote calls, want 1", remoteCalls)
	}

	now = now.Add(31 * time.Second)
	if key, _, reason := r.project(context.Background(), cwd); key != "example.com/owner/recovered" || reason != "" {
		t.Fatalf("retried remote resolve = key %q, reason %q", key, reason)
	}
	if remoteCalls != 2 {
		t.Fatalf("expired failure made %d remote calls, want 2", remoteCalls)
	}
}

func TestResolverCacheFingerprintFailureIsFailOpenForBoundedTTL(t *testing.T) {
	cwd := t.TempDir()
	configPath := filepath.Join(cwd, "git-config")
	if err := os.WriteFile(configPath, []byte("config"), 0o600); err != nil {
		t.Fatal(err)
	}
	remote := "git@example.com:owner/first.git"
	calls := 0
	git := func(_ context.Context, _ string, args ...string) (string, error) {
		calls++
		switch {
		case hasArg(args, "--is-inside-work-tree"):
			return "true", nil
		case hasArg(args, "--git-path"):
			return configPath, nil
		case args[0] == "remote":
			return remote, nil
		default:
			return "", fmt.Errorf("unexpected git arguments: %v", args)
		}
	}
	r := NewWith(git, nil)
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }
	r.cacheFallbackTTL = time.Minute

	if key, _, _ := r.project(context.Background(), cwd); key != "example.com/owner/first" {
		t.Fatalf("first key = %q", key)
	}
	if err := os.Remove(configPath); err != nil {
		t.Fatal(err)
	}
	remote = "git@example.com:owner/second.git"
	now = now.Add(30 * time.Second)
	if key, _, _ := r.project(context.Background(), cwd); key != "example.com/owner/first" {
		t.Fatalf("transient fingerprint failure discarded cached key: %q", key)
	}
	if calls != 3 {
		t.Fatalf("transient fingerprint failure ran git %d times, want 3", calls)
	}

	now = now.Add(61 * time.Second)
	if key, _, _ := r.project(context.Background(), cwd); key != "example.com/owner/second" {
		t.Fatalf("expired fallback key = %q, want refreshed remote", key)
	}
	if calls != 6 {
		t.Fatalf("git calls after fallback expiry = %d, want 6", calls)
	}
}

func TestResolverCacheRefreshesNoOriginAfterGitConfigChanges(t *testing.T) {
	cwd := t.TempDir()
	configPath := filepath.Join(cwd, "git-config")
	if err := os.WriteFile(configPath, []byte("no origin"), 0o600); err != nil {
		t.Fatal(err)
	}
	hasOrigin := false
	git := func(_ context.Context, _ string, args ...string) (string, error) {
		switch {
		case hasArg(args, "--is-inside-work-tree"):
			return "true", nil
		case hasArg(args, "--git-path"):
			return configPath, nil
		case hasArg(args, "--git-common-dir"):
			return filepath.Join(cwd, ".git"), nil
		case args[0] == "remote" && !hasOrigin:
			return "", fmt.Errorf("no such remote")
		case args[0] == "remote":
			return "git@example.com:owner/repo.git", nil
		default:
			return "", fmt.Errorf("unexpected git arguments: %v", args)
		}
	}
	r := NewWith(git, nil)

	if key, _, reason := r.project(context.Background(), cwd); key != "" || reason == "" {
		t.Fatalf("no-origin resolve = key %q, reason %q", key, reason)
	}
	hasOrigin = true
	if err := os.WriteFile(configPath, []byte("origin added with a different size"), 0o600); err != nil {
		t.Fatal(err)
	}
	if key, _, reason := r.project(context.Background(), cwd); key != "example.com/owner/repo" || reason != "" {
		t.Fatalf("post-origin resolve = key %q, reason %q", key, reason)
	}
}
