package httpapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"github.com/gofrs/flock"
)

const (
	// The SDK currently materializes every JSON-RPC message before decoding it.
	// This ceiling therefore bounds both its eventual allocation and the bytes a
	// remote peer can make the server receive for one request.
	mcpMaxRequestBytes = int64(100 << 20)
	// Ordinary MCP requests are a few KiB. Keeping that common case in memory
	// avoids a filesystem round trip while large arguments move to disk early.
	mcpMemoryBodyBytes = int64(1 << 20)
	// Admission reserves the full possible spool size before reading. Four slots
	// bound aggregate temporary storage at 400 MiB and prevent queued requests
	// from retaining an in-memory prefix.
	mcpMaxConcurrentSpools = 4
	mcpSpoolFilePattern    = "request-*.body"
	mcpSpoolLockPattern    = "request-*.lock"
)

// mcpBodySpooler puts a hard boundary in front of the SDK's unbounded
// io.ReadAll. Bodies stay in memory only through mcpMemoryBodyBytes; larger
// bodies are rewound from an owner-only temporary file for the SDK.
type mcpBodySpooler struct {
	next        http.Handler
	dir         string
	slots       chan struct{}
	maxBytes    int64
	memoryBytes int64
}

func newMCPBodySpooler(next http.Handler) http.Handler {
	dir := filepath.Join(os.TempDir(), "akari-mcp-spool")
	spooler, err := newMCPBodySpoolerAt(next, dir, mcpMaxConcurrentSpools)
	if err != nil {
		slog.Error("initialize MCP request storage", "error", err)
		return unavailableMCPBodySpooler(next)
	}
	return spooler
}

func unavailableMCPBodySpooler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && isJSONBody(r.Header.Get("Content-Type")) {
			http.Error(w, "MCP request storage unavailable", http.StatusInsufficientStorage)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func newMCPBodySpoolerAt(next http.Handler, dir string, maxSpools int) (*mcpBodySpooler, error) {
	if maxSpools <= 0 {
		return nil, fmt.Errorf("MCP spool concurrency must be positive")
	}
	if err := secureSpoolDir(dir); err != nil {
		return nil, err
	}
	s := &mcpBodySpooler{
		next: next, dir: dir, slots: make(chan struct{}, maxSpools),
		maxBytes: mcpMaxRequestBytes, memoryBytes: mcpMemoryBodyBytes,
	}
	if err := s.removeAbandoned(); err != nil {
		return nil, err
	}
	return s, nil
}

func secureSpoolDir(dir string) error {
	// 0700 is a POSIX owner-only mode; on Windows it has no effect and the
	// directory keeps its default ACLs instead. Production servers are Linux.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create MCP spool directory: %w", err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("inspect MCP spool directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("MCP spool path is not a directory")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("protect MCP spool directory: %w", err)
	}
	return nil
}

// removeAbandoned reclaims files whose advisory lock disappeared with a dead
// process. A rolling restart can share the directory safely: files still owned
// by the old process remain locked and are left alone.
//
// A single leftover file that cannot be locked, stat'd, or removed (a stale
// NFS handle, a permission hiccup, another process racing the same sweep) is
// logged and skipped instead of aborting the sweep. One uncooperative file no
// longer holds the whole MCP endpoint at 507 until someone cleans it up by
// hand. Spooler construction still fails if the spool directory itself is
// unusable; that is checked separately by secureSpoolDir.
func (s *mcpBodySpooler) removeAbandoned() error {
	paths, err := filepath.Glob(filepath.Join(s.dir, mcpSpoolFilePattern))
	if err != nil {
		return fmt.Errorf("list abandoned MCP spools: %w", err)
	}
	for _, path := range paths {
		if err := removeAbandonedBody(path); err != nil {
			slog.Warn("skip abandoned MCP spool", "path", path, "error", err)
		}
	}
	lockPaths, err := filepath.Glob(filepath.Join(s.dir, mcpSpoolLockPattern))
	if err != nil {
		return fmt.Errorf("list abandoned MCP spool locks: %w", err)
	}
	for _, lockPath := range lockPaths {
		if err := removeAbandonedLock(lockPath); err != nil {
			slog.Warn("skip abandoned MCP spool lock", "path", lockPath, "error", err)
		}
	}
	return nil
}

// removeAbandonedBody removes a single spool body left behind by a dead
// process, provided its lock is free. A locked file belongs to a live
// request and is left alone (not an error).
func removeAbandonedBody(path string) error {
	lockPath := spoolLockPath(path)
	lock := flock.New(lockPath)
	locked, err := lock.TryLock()
	if err != nil {
		_ = lock.Close()
		return fmt.Errorf("lock abandoned MCP spool: %w", err)
	}
	if !locked {
		_ = lock.Close()
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		_ = lock.Unlock()
		_ = lock.Close()
		return fmt.Errorf("remove abandoned MCP spool: %w", err)
	}
	if err := lock.Unlock(); err != nil {
		_ = lock.Close()
		return fmt.Errorf("unlock abandoned MCP spool: %w", err)
	}
	if err := lock.Close(); err != nil {
		return fmt.Errorf("close abandoned MCP spool lock: %w", err)
	}
	_ = os.Remove(lockPath)
	return nil
}

// removeAbandonedLock removes a lock-file placeholder (see the invariant
// documented on mcpSpoolSink.spill) whose body never showed up, provided the
// lock is free. A body that already exists, or a lock still held, is left
// alone (not an error).
func removeAbandonedLock(lockPath string) error {
	bodyPath := spoolBodyPath(lockPath)
	if _, err := os.Stat(bodyPath); err == nil || !errors.Is(err, os.ErrNotExist) {
		return nil
	}
	lock := flock.New(lockPath)
	locked, err := lock.TryLock()
	if err != nil {
		_ = lock.Close()
		return fmt.Errorf("lock abandoned MCP spool marker: %w", err)
	}
	if !locked {
		_ = lock.Close()
		return nil
	}
	_ = lock.Unlock()
	_ = lock.Close()
	_ = os.Remove(lockPath)
	return nil
}

func spoolLockPath(bodyPath string) string {
	return bodyPath[:len(bodyPath)-len(".body")] + ".lock"
}

func spoolBodyPath(lockPath string) string {
	return lockPath[:len(lockPath)-len(".lock")] + ".body"
}

func (s *mcpBodySpooler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || !isJSONBody(r.Header.Get("Content-Type")) {
		s.next.ServeHTTP(w, r)
		return
	}
	if r.ContentLength > s.maxBytes {
		rejectOversizedMCPRequest(w, r)
		return
	}

	body, cleanup, err := s.prepareBody(r.Context(), r.Body)
	if err != nil {
		closeMCPRequestBody(r.Body)
		var storageErr *spoolStorageError
		switch {
		case errors.Is(err, errMCPRequestTooLarge):
			rejectOversizedMCPRequest(w, r)
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			return
		case errors.As(err, &storageErr):
			// Our own storage failed: temp/lock file creation, a spool write, or
			// the rewind before handoff. Log it at error, since fixing it is on us.
			slog.Error("spool MCP request", "error", err)
			http.Error(w, "MCP request storage unavailable", http.StatusInsufficientStorage)
		default:
			// io.CopyBuffer folds source-read and sink-write errors into one
			// return value; anything that is not a recognized storage failure is
			// an ordinary client-side read failure (connection reset, broken pipe
			// mid-upload), routine for large bodies. The client is usually gone
			// already, so a best-effort 400 is fine.
			slog.Warn("client body read failed", "error", err)
			http.Error(w, "MCP request body could not be read", http.StatusBadRequest)
		}
		return
	}
	closeMCPRequestBody(r.Body)
	r.Body = body
	defer cleanup()
	s.next.ServeHTTP(w, r)
}

func closeMCPRequestBody(body io.Closer) {
	if err := body.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		slog.Error("close MCP request body", "error", err)
	}
}

func isJSONBody(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	return err == nil && mediaType == "application/json"
}

var errMCPRequestTooLarge = errors.New("MCP request body exceeds 100 MiB")

// spoolStorageError marks a failure writing to the spooler's own storage (a
// temp/lock file, a spool file write, or the rewind before handoff) as
// distinct from an ordinary failure reading the client's body. io.CopyBuffer
// in prepareBody folds source-read and sink-write errors into a single
// return value; ServeHTTP unwraps this marker to tell which side is at
// fault and answer 507 (ours) or 400 (the client's) accordingly.
type spoolStorageError struct {
	err error
}

func (e *spoolStorageError) Error() string { return e.err.Error() }
func (e *spoolStorageError) Unwrap() error { return e.err }

func wrapSpoolStorageError(err error) error {
	if err == nil {
		return nil
	}
	return &spoolStorageError{err: err}
}

func rejectOversizedMCPRequest(w http.ResponseWriter, r *http.Request) {
	// Do not let net/http drain an attacker-controlled remainder for connection
	// reuse after the application has made its decision.
	r.Close = true
	w.Header().Set("Connection", "close")
	http.Error(w, "MCP request body exceeds 100 MiB", http.StatusRequestEntityTooLarge)
}

func (s *mcpBodySpooler) prepareBody(ctx context.Context, src io.Reader) (io.ReadCloser, func(), error) {
	sink := &mcpSpoolSink{owner: s}
	select {
	case s.slots <- struct{}{}:
		sink.acquired = true
	case <-ctx.Done():
		return nil, func() {}, ctx.Err()
	}
	limited := &io.LimitedReader{R: &contextReader{ctx: ctx, r: src}, N: s.maxBytes + 1}
	_, err := io.CopyBuffer(sink, limited, make([]byte, 32<<10))
	if err != nil {
		sink.cleanup()
		return nil, func() {}, err
	}
	if limited.N == 0 {
		sink.cleanup()
		return nil, func() {}, errMCPRequestTooLarge
	}
	body, err := sink.body()
	if err != nil {
		sink.cleanup()
		return nil, func() {}, err
	}
	return body, sink.cleanup, nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

type mcpSpoolSink struct {
	owner *mcpBodySpooler

	memory   bytes.Buffer
	file     *os.File
	lock     *flock.Flock
	lockPath string
	acquired bool
	once     sync.Once
}

func (s *mcpSpoolSink) Write(p []byte) (int, error) {
	if s.file != nil {
		n, err := s.file.Write(p)
		if err != nil {
			return n, wrapSpoolStorageError(fmt.Errorf("write MCP spool: %w", err))
		}
		return n, nil
	}
	if int64(s.memory.Len()+len(p)) <= s.owner.memoryBytes {
		return s.memory.Write(p)
	}
	if err := s.spill(); err != nil {
		return 0, err
	}
	n, err := s.file.Write(p)
	if err != nil {
		return n, wrapSpoolStorageError(fmt.Errorf("write MCP spool: %w", err))
	}
	return n, nil
}

func (s *mcpSpoolSink) spill() error {
	lockFile, err := os.CreateTemp(s.owner.dir, mcpSpoolLockPattern)
	if err != nil {
		return wrapSpoolStorageError(fmt.Errorf("create MCP spool lock: %w", err))
	}
	s.lockPath = lockFile.Name()
	if err := lockFile.Close(); err != nil {
		return wrapSpoolStorageError(fmt.Errorf("close MCP spool lock file: %w", err))
	}
	// Between the Close above and the TryLock below, this lock file exists on
	// disk but nothing holds it yet. If another process's startup sweep
	// (removeAbandoned) runs in that gap, it sees an unlocked lock file with
	// no matching body and will happily remove it as an orphan. Correctness
	// here rests on flock.New opening the lock path with os.O_CREATE, so the
	// TryLock below silently recreates the file if the sweep won the race. A
	// future flock upgrade that drops O_CREATE from its default open flags
	// would quietly reintroduce this race.
	s.lock = flock.New(s.lockPath)
	locked, err := s.lock.TryLock()
	if err != nil {
		return wrapSpoolStorageError(fmt.Errorf("lock MCP spool: %w", err))
	}
	if !locked {
		return wrapSpoolStorageError(fmt.Errorf("lock MCP spool: lock unavailable"))
	}
	bodyPath := spoolBodyPath(s.lockPath)
	// 0600 is a POSIX owner-only mode; on Windows it has no effect and the
	// file keeps its default ACLs instead. Production servers are Linux.
	file, err := os.OpenFile(bodyPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return wrapSpoolStorageError(fmt.Errorf("create MCP spool: %w", err))
	}
	s.file = file
	if err := file.Chmod(0o600); err != nil {
		return wrapSpoolStorageError(fmt.Errorf("protect MCP spool: %w", err))
	}
	if _, err := file.Write(s.memory.Bytes()); err != nil {
		return wrapSpoolStorageError(fmt.Errorf("write MCP spool: %w", err))
	}
	s.memory = bytes.Buffer{}
	return nil
}

func (s *mcpSpoolSink) body() (io.ReadCloser, error) {
	if s.file == nil {
		<-s.owner.slots
		s.acquired = false
		return io.NopCloser(bytes.NewReader(s.memory.Bytes())), nil
	}
	if _, err := s.file.Seek(0, io.SeekStart); err != nil {
		return nil, wrapSpoolStorageError(fmt.Errorf("rewind MCP spool: %w", err))
	}
	return s.file, nil
}

func (s *mcpSpoolSink) cleanup() {
	s.once.Do(func() {
		if s.file != nil {
			name := s.file.Name()
			if err := s.file.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
				slog.Error("close MCP request spool", "error", err)
			}
			if err := os.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
				slog.Error("remove MCP request spool", "error", err)
			}
		}
		if s.lock != nil {
			if err := s.lock.Unlock(); err != nil {
				slog.Error("unlock MCP request spool", "error", err)
			}
			if err := s.lock.Close(); err != nil {
				slog.Error("close MCP request spool lock", "error", err)
			}
		}
		if s.lockPath != "" {
			if err := os.Remove(s.lockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				slog.Error("remove MCP request spool lock", "error", err)
			}
		}
		if s.acquired {
			<-s.owner.slots
		}
	})
}
