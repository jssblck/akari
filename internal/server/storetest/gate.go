package storetest

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand/v2"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofrs/flock"
)

// Connection budget. Every test provisions its own database and opens a pool
// against the one shared Postgres, so the suite's peak connection load is the
// number of test Stores live at once times the per-pool cap. Two constants bound
// that product:
//
//   - poolMaxConns (C) caps a single Store's pool.
//   - maxLiveStores (N) caps how many Stores are live at once, and it does so
//     ACROSS every `go test` binary, not just within one package's process. The
//     storeGate below is a counting semaphore backed by advisory file locks in a
//     temp directory keyed to the Postgres instance, so every binary aimed at that
//     server draws from the same N slots. A plain in-process semaphore cannot do
//     this (each package `go test` compiles is its own binary with its own memory),
//     which is why `go test ./...` no longer needs a `-p` pin to stay bounded.
//
// The ceiling is N*C = 32*4 = 128 connections, under the 200 max_connections .eph
// configures, leaving room for the transient CREATE/DROP maintenance connections
// and the dev server's pool under `eph run`. It holds regardless of `-p` or
// GOMAXPROCS, since the gate spans processes. Set AKARI_TEST_MAX_STORES to tune N
// (a smaller Postgres wants a smaller N; more headroom allows a larger one).
//
// As before, the ceiling is a worst case, not the steady state: pgx opens pool
// connections lazily (pool_min_conns defaults to 0), so a pool holds only what its
// test uses at once. Almost every test sits at one or two connections and only the
// concurrent-ingest test reaches C, so a full run peaks well under the ceiling.
const (
	// poolMaxConns caps one test Store's pool. It is a ceiling, not a reservation:
	// because pgx opens connections lazily, a test holds only as many as it uses at
	// once, which for almost every test is one or two. The cap of 4 is for the few
	// tests that drive real concurrency (TestRunEndToEnd runs a four-worker ingest
	// against its Store).
	poolMaxConns = 4

	// defaultMaxLiveStores is N: the number of concurrent live Stores the gate
	// admits across every test binary sharing one Postgres. EnvMaxLiveStores
	// overrides it. Keep N*poolMaxConns under the server's max_connections.
	defaultMaxLiveStores = 32

	// EnvMaxLiveStores overrides defaultMaxLiveStores, so a run against a Postgres
	// with a different connection ceiling can retune the gate without a recompile.
	EnvMaxLiveStores = "AKARI_TEST_MAX_STORES"
)

// gate is the process's handle on the shared cross-process semaphore, built once
// on first use and keyed to the Postgres instance the tests target.
var (
	gateMu sync.Mutex
	gate   *storeGate
)

// storeGate is a counting semaphore of n slots shared across processes through
// advisory file locks: one lock file per slot in a shared directory. Acquiring a
// slot means holding an exclusive flock on one of those files, which the OS
// releases automatically when the holding process exits, so a killed or timed-out
// test binary never wedges the suite with a leaked slot (the failure mode a
// hand-rolled counter file would have).
type storeGate struct {
	dir   string
	n     int
	start atomic.Uint64 // rotates each acquirer's first slot to spread contention
}

// acquireStoreSlot blocks until it holds one of the shared gate's slots, then
// registers the slot's release. Being registered first (before provision's drop
// cleanup and NewStore's Store.Close), it runs last, so a slot spans a test's
// entire Postgres footprint rather than just the window its pool is open.
func acquireStoreSlot(t *testing.T, base string) {
	t.Helper()
	lock := sharedStoreGate(t, base).acquire()
	t.Cleanup(func() {
		if err := lock.Unlock(); err != nil {
			t.Errorf("release store gate slot: %v", err)
		}
	})
}

// sharedStoreGate returns the process-wide gate, building it on first call. Every
// test uses the same base (the one AKARI_TEST_DATABASE_URL), so the gate is built
// once and reused.
func sharedStoreGate(t *testing.T, base string) *storeGate {
	t.Helper()
	gateMu.Lock()
	defer gateMu.Unlock()
	if gate == nil {
		g, err := newStoreGate(gateDir(base), maxLiveStoresFromEnv(t))
		if err != nil {
			t.Fatalf("build store gate: %v", err)
		}
		gate = g
	}
	return gate
}

// maxLiveStoresFromEnv reads N from EnvMaxLiveStores, defaulting when it is unset.
func maxLiveStoresFromEnv(t *testing.T) int {
	t.Helper()
	v := os.Getenv(EnvMaxLiveStores)
	if v == "" {
		return defaultMaxLiveStores
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		t.Fatalf("%s=%q: want a positive integer", EnvMaxLiveStores, v)
	}
	return n
}

// gateDir is a temp directory unique to a Postgres instance, derived from the
// host and port in base. Every test binary aimed at that server resolves the same
// directory and so shares one gate, while runs against a different server (another
// worktree's eph Postgres on its own random port) hash to a different directory
// and are bounded independently. The resource being protected is that one server,
// so keying on it, not on the workspace, is what makes the bound correct when two
// runs happen to share an instance.
func gateDir(base string) string {
	key := "default"
	if u, err := url.Parse(base); err == nil && u.Host != "" {
		key = u.Host // host:port
	}
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(os.TempDir(), "akari-storetest-gate-"+hex.EncodeToString(sum[:8]))
}

// newStoreGate creates the gate's slot directory. The lock files themselves are
// created lazily by the first acquirer to touch each slot.
func newStoreGate(dir string, n int) (*storeGate, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create gate dir %q: %w", dir, err)
	}
	return &storeGate{dir: dir, n: n}, nil
}

// acquire blocks until it locks one of the gate's slots and returns the held lock;
// the caller releases the slot by unlocking it. It never gives up because the
// suite always makes forward progress: every held slot is released on its test's
// cleanup, so a slot frees eventually. An acquire that outlives its test dies with
// the test binary (which releases every lock it holds), so a genuine stall surfaces
// as a `go test` timeout rather than a silent hang.
func (g *storeGate) acquire() *flock.Flock {
	for {
		start := g.start.Add(1)
		for i := 0; i < g.n; i++ {
			slot := int((start + uint64(i)) % uint64(g.n))
			fl := flock.New(filepath.Join(g.dir, "slot-"+strconv.Itoa(slot)))
			locked, err := fl.TryLock()
			if err == nil && locked {
				return fl
			}
			// Contended or errored: drop the fd before trying the next slot so a
			// busy sweep does not leak file handles.
			_ = fl.Close()
		}
		// Every slot was busy. Back off with jitter so separate processes sweeping
		// in lockstep do not keep colliding on the same slots.
		time.Sleep(time.Duration(2+rand.IntN(8)) * time.Millisecond)
	}
}
