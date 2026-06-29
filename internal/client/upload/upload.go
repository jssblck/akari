// Package upload drives the ingest protocol from the client side, statelessly.
// The server is authoritative for how many bytes of each file it holds, so the
// client persists nothing: it announces, reconciles against the server's cursor
// and content hash, and streams the gap in newline-terminated chunks.
package upload

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/jssblck/akari/internal/parser"
)

// hardCap bounds the largest single transformed message (one rewritten JSONL
// line) the client will buffer. After the transform lifts tool bodies to the CAS
// a line is small, so this only refuses a truly pathological line; it matches the
// server's maxChunk, bounding the server's worst-case parse memory too. It is a
// var so tests can shrink it to exercise the refusal path.
var hardCap int64 = 128 << 20

// settleWindow is how long a session file must be idle before the client uploads
// its trailing in-progress turn. A Codex turn closes only at the next user line,
// so a session's final turn has no closer; once the file stops growing, the turn
// is done and is flushed. It is a var so tests can force an immediate flush.
var settleWindow = 60 * time.Second

// Action describes what SyncFile did for a file.
type Action string

const (
	ActionUpToDate Action = "uptodate" // server already had the whole file
	ActionUploaded Action = "uploaded" // appended new bytes
	ActionReset    Action = "reset"    // diverged, re-uploaded from zero
)

// Target is everything needed to upload one resolved session file. Kind is the
// session's classification ("remote", "standalone", or "orphaned"). ProjectKey
// is set only for a remote session; for standalone and orphaned sessions the
// server derives the project key from Machine and Cwd.
type Target struct {
	Agent      string
	Path       string
	SourceID   string
	Kind       string
	ProjectKey string
	GitBranch  string
	Cwd        string
	Machine    string
}

// Outcome reports the result of syncing one file.
type Outcome struct {
	Action        Action
	UploadedBytes int64
	StoredBytes   int64
	MessageCount  int
}

// Client talks to one akari server with one bearer token.
type Client struct {
	http    *http.Client
	baseURL string
	token   string

	// files caches per-path incremental sync state so that re-syncing a growing
	// session does work proportional to the newly appended bytes, not to the whole
	// file. It is guarded by mu; a given path is synced single-flight, so the
	// *fileSync it returns is only ever touched by one sync at a time.
	mu    sync.Mutex
	files map[string]*fileSync
}

// fileSync is the per-file state that keeps repeated syncs of an append-only
// session O(newly appended bytes). It is a cache only: dropping it (a restart, a
// fresh Client) costs a one-time full re-hash and re-transform, never
// correctness, and any sign the file diverged from what it describes forces that
// full path.
//
// Under the client-CAS protocol the bytes the server stores are the TRANSFORMED
// transcript (tool bodies lifted to the CAS, replaced by sentinels), so the
// verified-prefix cache is kept over the transformed stream: base is the count of
// transformed bytes the server holds, prefixHasher is the sha256 of those
// transformed bytes, and origBase is the count of original on-disk bytes that
// transform to them. The client still resumes by the original file (it is what it
// can recompute statelessly), so origBase is where the next transform pass reads
// from.
type fileSync struct {
	// lock serializes the whole sync of one path. The fields below and the hasher
	// are not safe for concurrent use, so SyncFile holds it for its entire run; two
	// goroutines syncing the same path proceed one at a time, while different paths
	// (different fileSync) run in parallel. It is a one-slot semaphore rather than a
	// sync.Mutex so the wait can be abandoned when the caller's context is canceled:
	// the holder may be parked on a slow HTTP call, and a mutex wait would ignore a
	// shutdown. A nil channel (a fileSync built directly in a test) means no locking.
	lock chan struct{}

	// Verified prefix, over the transformed stream. The server holds transformed
	// bytes [0, base); prefixHasher has consumed exactly those bytes, so the next
	// verification compares the cached digest (an append) instead of re-transforming
	// from zero. origBase is the original-file offset that produced base transformed
	// bytes, so the next transform pass reads original [origBase, size). prefixSize
	// is the original file size observed at the last verification; a file shorter
	// than that has been truncated, so the cheap path is abandoned for a full
	// re-transform.
	base         int64
	origBase     int64
	prefixHasher hash.Hash
	prefixSize   int64
}

// New builds a Client. baseURL is the server root (trailing slash optional).
func New(httpClient *http.Client, baseURL, token string) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{
		http:    httpClient,
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		files:   map[string]*fileSync{},
	}
}

// fileState returns the cached sync state for a path, creating an empty one (cold
// cache: base 0, no hasher) on first use.
func (c *Client) fileState(path string) *fileSync {
	c.mu.Lock()
	defer c.mu.Unlock()
	fs := c.files[path]
	if fs == nil {
		fs = &fileSync{lock: make(chan struct{}, 1)}
		c.files[path] = fs
	}
	return fs
}

// rewind sets the verified prefix back to empty: the server holds nothing, so the
// whole file will re-transform and re-upload from zero. Both the transformed
// cursor and the original cursor reset, since with nothing accepted there is no
// verified original prefix to resume from.
func (fs *fileSync) rewind(size int64) {
	fs.base = 0
	fs.origBase = 0
	fs.prefixHasher = sha256.New()
	fs.prefixSize = size
}

// maxConflictRetries bounds how many times a single SyncFile re-announces after
// an offset conflict, so an unexpected server-state race cannot loop forever.
const maxConflictRetries = 5

// SyncFile announces a file, reconciles against the server's cursor, and uploads
// any new complete lines. It is safe to call repeatedly: an up-to-date file moves
// no bytes.
//
// A mid-stream offset conflict (HTTP 409) means the server's cursor moved out
// from under us, so the prefix verified at announce is stale. Rather than trust
// the conflict's reported cursor blindly (which could append onto a divergent
// prefix), SyncFile re-announces and re-verifies the prefix from scratch, up to
// maxConflictRetries times.
func (c *Client) SyncFile(ctx context.Context, t Target) (Outcome, error) {
	f, err := os.Open(t.Path)
	if err != nil {
		return Outcome{}, err
	}
	defer f.Close()

	// Hold the per-file lock for the whole sync so concurrent SyncFile calls for the
	// same path serialize instead of racing on the shared cursor and hasher. The
	// acquire is cancellable: a caller whose context is canceled (a shutdown) stops
	// waiting instead of blocking behind an in-flight sync that may be stuck on I/O.
	fs := c.fileState(t.Path)
	select {
	case fs.lock <- struct{}{}:
		defer func() { <-fs.lock }()
	case <-ctx.Done():
		return Outcome{}, ctx.Err()
	}

	var totalUploaded int64
	sawReset := false
	for attempt := 0; attempt < maxConflictRetries; attempt++ {
		out, conflicted, err := c.syncOnce(ctx, t, fs, f)
		if err != nil {
			return Outcome{}, err
		}
		totalUploaded += out.UploadedBytes
		if out.Action == ActionReset {
			sawReset = true
		}
		if !conflicted {
			// Roll up work across all retries so the caller's summary is accurate.
			out.UploadedBytes = totalUploaded
			switch {
			case sawReset:
				out.Action = ActionReset
			case totalUploaded > 0:
				out.Action = ActionUploaded
			}
			return out, nil
		}
	}
	return Outcome{}, fmt.Errorf("sync %s: too many offset conflicts (%d attempts)", t.Path, maxConflictRetries)
}

// syncOnce performs one announce, reconcile, and upload pass. It returns
// conflicted=true if an offset conflict interrupted the upload, signalling the
// caller to re-announce and try again.
func (c *Client) syncOnce(ctx context.Context, t Target, fs *fileSync, f *os.File) (Outcome, bool, error) {
	ann, err := c.announce(ctx, t)
	if err != nil {
		return Outcome{}, false, err
	}
	info, err := f.Stat()
	if err != nil {
		return Outcome{}, false, err
	}
	size := info.Size()
	// A session's final turn has no closing user line, so it is withheld until the
	// file goes idle. Treat a file untouched for settleWindow as settled, which
	// also keeps a turn that merely paused mid-write from being flushed early.
	settled := time.Since(info.ModTime()) > settleWindow

	action := ActionUploaded
	if ann.StoredBytes == 0 {
		// The server holds nothing: there is no prefix to verify.
		fs.rewind(size)
	} else {
		ok, err := c.verifyPrefix(f, fs, t.Agent, ann.StoredBytes, size, ann.PrefixSHA256)
		if err != nil {
			return Outcome{}, false, err
		}
		if !ok {
			// The local file was rotated, truncated, or rewritten, or the server
			// diverged: drop the server copy and re-upload from zero.
			if err := c.reset(ctx, ann.SessionID); err != nil {
				return Outcome{}, false, err
			}
			fs.rewind(size)
			action = ActionReset
		}
	}

	out := Outcome{StoredBytes: ann.StoredBytes}

	// Transform the unsent original tail into boundary-aligned transformed chunks,
	// lifting every tool body to the CAS. Each pass reads original [origBase, size);
	// origBase advances only past bytes the server accepts, so steady-state work is
	// proportional to the newly appended bytes. A withheld trailing turn leaves
	// origBase where it is and is re-examined next tick (the only repeated work, and
	// only for an open final turn).
	res, err := transformTail(f, fs.origBase, size, t.Agent, settled)
	if err != nil {
		return out, false, err
	}
	for _, ch := range res.chunks {
		// Upload the bodies this chunk references before the chunk itself, so the
		// transcript never lands referencing a body the CAS does not yet hold.
		if err := c.uploadBodies(ctx, ch.Bodies); err != nil {
			return out, false, err
		}
		r, err := c.chunk(ctx, ann.SessionID, fs.base, ch.Data)
		if err != nil {
			return out, false, err
		}
		if r.conflict {
			return out, true, nil // re-announce and re-verify the prefix
		}
		// The server accepted these transformed bytes, so extend the verified
		// transformed-prefix digest over them (hashing the chunk we already hold) and
		// advance the original cursor past the bytes they came from.
		fs.prefixHasher.Write(ch.Data)
		fs.base = r.storedBytes
		fs.origBase += ch.OrigLen
		fs.prefixSize = size
		out.UploadedBytes += int64(len(ch.Data))
		out.StoredBytes = r.storedBytes
		out.MessageCount = r.messageCount
	}

	if out.UploadedBytes == 0 && action != ActionReset {
		action = ActionUpToDate
	}
	out.Action = action
	return out, false, nil
}

// boundaryWithin returns the absolute file position just past the last message
// boundary in buf (which begins at file position bufStart and ends on a newline),
// or 0 when buf holds none. For Claude and pi every newline is a boundary, so it
// is the buffer's end; for Codex it is the last turn close (a user line).
func boundaryWithin(buf []byte, bufStart int64, agent string) int64 {
	if agent != agentCodex {
		return bufStart + int64(len(buf))
	}
	if rel := lastCodexTurnEnd(buf); rel > 0 {
		return bufStart + int64(rel)
	}
	return 0
}

func errMessageTooBig(agent string) error {
	return fmt.Errorf("session %s message exceeds %d bytes without a boundary", agent, hardCap)
}

// readAt fills buf entirely from offset off. A short read means the file was
// truncated or rotated between Stat and now, so the missing bytes would otherwise
// be read as zero-filled session content; readAt treats any short read as an error
// (including the io.EOF ReadAt returns for one) and wraps it with the range. A full
// read returns nil even at the exact end of file, where ReadAt may report io.EOF.
func readAt(f *os.File, buf []byte, off int64) error {
	n, err := f.ReadAt(buf, off)
	if n == len(buf) {
		return nil
	}
	if err == nil {
		err = io.ErrUnexpectedEOF
	}
	return fmt.Errorf("read session file [%d,%d): short read (%d of %d bytes): %w",
		off, off+int64(len(buf)), n, len(buf), err)
}

// agentCodex is the agent string whose sessions fold a turn across lines, so a
// chunk boundary is a turn close (a user line) rather than any newline.
const agentCodex = "codex"

// codexLine is the minimal shape needed to spot a turn boundary: a Codex turn
// closes at the next user entry, so a response_item carrying role "user" ends the
// preceding turn.
type codexLine struct {
	Type    string `json:"type"`
	Payload struct {
		Role string `json:"role"`
	} `json:"payload"`
}

// lastCodexTurnEnd returns the offset just past the last line that closes a turn
// (a response_item with role "user"), or 0 when the buffer holds no such line.
// Cutting there keeps every folded turn whole within one chunk: the user entry
// that closes a turn travels with it, and the next turn begins in the next chunk.
func lastCodexTurnEnd(buf []byte) int {
	last := 0
	start := 0
	for i := 0; i < len(buf); i++ {
		if buf[i] != '\n' {
			continue
		}
		var cl codexLine
		if json.Unmarshal(bytes.TrimSpace(buf[start:i]), &cl) == nil &&
			cl.Type == "response_item" && cl.Payload.Role == "user" {
			last = i + 1
		}
		start = i + 1
	}
	return last
}

// verifyPrefix reports whether the TRANSFORMED prefix of the local file matches
// the server's stored transformed bytes, and advances fs to record that
// verification. The server holds transformed bytes (tool bodies lifted to the
// CAS), so the comparison is against the transformed prefix, not the raw file.
//
// The fast path is the common one: an append-only file whose transformed prefix
// the cache already covers exactly (fs.base == serverBytes) is confirmed by
// comparing the cached digest with no I/O. Otherwise (a cold cache after a
// restart, a server rewind, a concurrent writer that advanced the cursor, or a
// truncation) the client re-transforms the original file from zero until the
// transformed output reaches serverBytes, hashing as it goes and recovering the
// origBase mapping. That cold pass re-reads and re-hashes the bodies in the prefix
// once, the documented cost of a dropped cache, but never re-uploads them.
func (c *Client) verifyPrefix(f *os.File, fs *fileSync, agent string, serverBytes, size int64, want string) (bool, error) {
	// serverBytes counts TRANSFORMED bytes and size counts ORIGINAL file bytes, so
	// they are not directly comparable: a sentinel can be larger or smaller than the
	// body it replaces. The fast path's guard is on the original coordinate: the
	// file must still hold at least the original bytes the cache already consumed
	// (origBase). A file shorter than that was truncated and drops to the cold path.
	if fs.prefixHasher != nil && fs.base == serverBytes && size >= fs.origBase && size >= fs.prefixSize {
		// Append-only growth with the transformed prefix already hashed: compare the
		// cached digest.
		fs.prefixSize = size
		return hex.EncodeToString(fs.prefixHasher.Sum(nil)) == want, nil
	}

	// Cold path: re-transform the original file from zero until the transformed
	// output reaches serverBytes, computing both the digest and the original offset
	// that maps to it. The transform is deterministic, so the recomputed prefix is
	// byte identical to what was uploaded.
	h, origBase, ok, err := transformPrefixDigest(f, agent, size, serverBytes)
	if err != nil {
		return false, err
	}
	if !ok || hex.EncodeToString(h.Sum(nil)) != want {
		return false, nil
	}
	fs.prefixHasher = h
	fs.base = serverBytes
	fs.origBase = origBase
	fs.prefixSize = size
	return true, nil
}

type announceResp struct {
	SessionID    int64  `json:"session_id"`
	StoredBytes  int64  `json:"stored_bytes"`
	PrefixSHA256 string `json:"prefix_sha256"`
}

func (c *Client) announce(ctx context.Context, t Target) (announceResp, error) {
	body := map[string]string{
		"agent":             t.Agent,
		"source_session_id": t.SourceID,
		"kind":              t.Kind,
		"project_remote":    t.ProjectKey,
		"git_branch":        t.GitBranch,
		"cwd":               t.Cwd,
		"machine":           t.Machine,
	}
	var out announceResp
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/ingest/session", body, &out); err != nil {
		return announceResp{}, err
	}
	return out, nil
}

func (c *Client) reset(ctx context.Context, sessionID int64) error {
	path := fmt.Sprintf("/api/v1/ingest/session/%d/reset", sessionID)
	return c.doJSON(ctx, http.MethodPost, path, nil, nil)
}

type chunkResult struct {
	storedBytes  int64
	messageCount int
	conflict     bool
}

func (c *Client) chunk(ctx context.Context, sessionID, offset int64, data []byte) (chunkResult, error) {
	url := fmt.Sprintf("%s/api/v1/ingest/session/%d/chunk?offset=%d", c.baseURL, sessionID, offset)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return chunkResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := c.http.Do(req)
	if err != nil {
		return chunkResult{}, err
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	switch resp.StatusCode {
	case http.StatusOK:
		var r struct {
			StoredBytes  int64 `json:"stored_bytes"`
			MessageCount int   `json:"message_count"`
		}
		if err := json.Unmarshal(payload, &r); err != nil {
			return chunkResult{}, fmt.Errorf("decode chunk response: %w", err)
		}
		return chunkResult{storedBytes: r.StoredBytes, messageCount: r.MessageCount}, nil
	case http.StatusConflict:
		var r struct {
			StoredBytes int64 `json:"stored_bytes"`
		}
		if err := json.Unmarshal(payload, &r); err != nil {
			return chunkResult{}, fmt.Errorf("decode conflict response: %w", err)
		}
		return chunkResult{storedBytes: r.StoredBytes, conflict: true}, nil
	default:
		return chunkResult{}, fmt.Errorf("chunk: server returned %d: %s", resp.StatusCode, strings.TrimSpace(string(payload)))
	}
}

// uploadBodies uploads the tool bodies a transformed chunk references, sending
// only the ones the server lacks. It asks the server which hashes are missing
// (the CAS dedupes globally, so a body any session already stored is skipped),
// then streams each missing body to the CAS under its hash. A body is uploaded
// before the transcript that references it, and the server pins it against the
// sweep, so the reference can never land on a body the CAS does not hold.
func (c *Client) uploadBodies(ctx context.Context, bodies []parser.Body) error {
	if len(bodies) == 0 {
		return nil
	}
	shas := make([]string, len(bodies))
	for i, b := range bodies {
		shas[i] = b.SHA256
	}
	missing, err := c.checkBlobs(ctx, shas)
	if err != nil {
		return err
	}
	uploaded := map[string]bool{}
	for _, b := range bodies {
		if !missing[b.SHA256] || uploaded[b.SHA256] {
			continue // already present on the server, or already sent this pass
		}
		if err := c.putBlob(ctx, b); err != nil {
			return err
		}
		uploaded[b.SHA256] = true
	}
	return nil
}

// checkBlobs returns the set of hashes the server does not yet hold.
func (c *Client) checkBlobs(ctx context.Context, shas []string) (map[string]bool, error) {
	var resp struct {
		Missing []string `json:"missing"`
	}
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/ingest/blobs/check",
		map[string]any{"sha256": shas}, &resp); err != nil {
		return nil, err
	}
	missing := make(map[string]bool, len(resp.Missing))
	for _, sha := range resp.Missing {
		missing[sha] = true
	}
	return missing, nil
}

// putBlob streams one tool body to the CAS. The body is held in memory as a
// string (one line's worth, the size of a single tool input or result), so the
// request streams from it without a second copy; a body larger than one line never
// arises, since the transform lifts each body from its own line.
func (c *Client) putBlob(ctx context.Context, b parser.Body) error {
	url := fmt.Sprintf("%s/api/v1/ingest/blob/%s?media_type=%s",
		c.baseURL, b.SHA256, urlQueryEscape(b.MediaType))
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, strings.NewReader(b.Content))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.ContentLength = int64(len(b.Content))

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("upload blob %s: server returned %d: %s", b.SHA256, resp.StatusCode, strings.TrimSpace(string(payload)))
	}
	return nil
}

// urlQueryEscape escapes a value for use in a query string.
func urlQueryEscape(s string) string { return url.QueryEscape(s) }

// doJSON performs a JSON request, optionally decoding a JSON response into out.
func (c *Client) doJSON(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: server returned %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(payload)))
	}
	if out != nil {
		if err := json.Unmarshal(payload, out); err != nil {
			return fmt.Errorf("decode %s response: %w", path, err)
		}
	}
	return nil
}
