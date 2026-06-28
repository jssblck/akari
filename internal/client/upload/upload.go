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
	"os"
	"strings"
	"sync"
	"time"
)

// chunkTarget is the preferred chunk size. A chunk is trimmed back to a message
// boundary (a newline for Claude and pi, a turn boundary for Codex) so it carries
// only whole messages. Well-behaved small sessions move in sub-megabyte chunks.
const chunkTarget = 1 << 20

// hardCap bounds how far the client will grow a chunk to reach a boundary when a
// single message (a JSONL line, or a folded Codex turn) is larger than
// chunkTarget. A message that big is served alone, as one oversized chunk; only a
// truly pathological size is refused. It matches the server's maxChunk, and since
// the server parses one oversized chunk in one region, it also bounds the server's
// worst-case parse memory and the largest buffer the client allocates per chunk.
// It is a var so tests can shrink it to exercise the refusal path.
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
// fresh Client) costs a one-time full re-hash and re-scan, never correctness, and
// any sign the file diverged from what it describes forces that full path.
type fileSync struct {
	// lock serializes the whole sync of one path. The fields below and the hasher
	// are not safe for concurrent use, so SyncFile holds it for its entire run; two
	// goroutines syncing the same path proceed one at a time, while different paths
	// (different fileSync) run in parallel. It is a one-slot semaphore rather than a
	// sync.Mutex so the wait can be abandoned when the caller's context is canceled:
	// the holder may be parked on a slow HTTP call, and a mutex wait would ignore a
	// shutdown. A nil channel (a fileSync built directly in a test) means no locking.
	lock chan struct{}

	// Verified prefix. Local bytes [0, base) are confirmed to match what the server
	// stored, and prefixHasher has consumed exactly those bytes, so the next
	// verification extends the digest over only the newly stored bytes instead of
	// rehashing from zero. prefixSize is the file size observed at the last
	// verification; a file shorter than that has been truncated, so the cheap path
	// is abandoned for a full re-hash.
	base         int64
	prefixHasher hash.Hash
	prefixSize   int64

	// Scan cursor. Bytes [base, scanned) were already examined for a message
	// boundary and hold none, so boundary detection resumes at scanned and reads
	// only the newly appended tail. It is always >= base.
	scanned int64
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
// whole file will re-upload from zero. It keeps the scan cursor only when the bytes
// it covers are both unsent and unchanged: base was already 0 (nothing had been
// accepted, so [0, scanned) is still a boundary-free unsent prefix) and the file
// only grew. Otherwise (we had uploaded bytes whose boundaries must be re-found, or
// the file shrank under us) it rescans from zero.
func (fs *fileSync) rewind(size int64) {
	keepScan := fs.base == 0 && size >= fs.prefixSize
	fs.base = 0
	fs.prefixHasher = sha256.New()
	fs.prefixSize = size
	if !keepScan {
		fs.scanned = 0
	}
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
		// The server holds nothing: there is no prefix to verify. rewind keeps the
		// scan cursor when the file is just append-growing before its first chunk is
		// uploadable, so a withheld opening turn is not rescanned from zero each tick.
		fs.rewind(size)
	} else {
		ok, err := c.verifyPrefix(f, fs, ann.StoredBytes, size, ann.PrefixSHA256)
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

	if fs.scanned < fs.base {
		fs.scanned = fs.base
	}
	out := Outcome{StoredBytes: ann.StoredBytes}
	for fs.base < size {
		chunk, scannedTo, err := nextChunk(f, fs.base, fs.scanned, size, t.Agent, settled)
		if err != nil {
			return out, false, err
		}
		if len(chunk) == 0 {
			// Only an incomplete or in-progress trailing message remains. Remember how
			// far we scanned so the next tick resumes from there instead of rescanning.
			fs.scanned = scannedTo
			break
		}
		res, err := c.chunk(ctx, ann.SessionID, fs.base, chunk)
		if err != nil {
			return out, false, err
		}
		if res.conflict {
			return out, true, nil // re-announce and re-verify the prefix
		}
		// The server accepted these bytes, so extend the verified prefix over them
		// (hashing the chunk we already hold, not re-reading the file) and resume
		// scanning from the new send position.
		fs.prefixHasher.Write(chunk)
		fs.base = res.storedBytes
		fs.prefixSize = size
		fs.scanned = fs.base
		out.UploadedBytes += int64(len(chunk))
		out.StoredBytes = res.storedBytes
		out.MessageCount = res.messageCount
	}

	if out.UploadedBytes == 0 && action != ActionReset {
		action = ActionUpToDate
	}
	out.Action = action
	return out, false, nil
}

// nextChunk returns the bytes to send next, [base, boundary), so a chunk never
// splits a message. For Claude and pi a message is one JSONL line, so the boundary
// is the last newline. For Codex a message is a folded turn (reasoning, tool
// calls, and the assistant reply), so the boundary is the last turn end, which
// keeps a turn from spanning a chunk (and so a parse region).
//
// scanFrom is where boundary detection resumes: bytes in [base, scanFrom) were
// examined on an earlier tick and hold no boundary, and the file only appends, so
// only [scanFrom, size) needs scanning. nextChunk returns the chunk and scannedTo,
// how far it got, which the caller caches as the next scanFrom. A nil chunk means
// nothing is completable yet: an unfinished trailing line, or, for Codex, a
// trailing in-progress turn whose closing user line has not arrived and whose file
// has not settled. settled lets the final turn of an idle session be flushed even
// without that closing line.
//
// The open region [base, size) is held to hardCap: a single message that grows
// past it across ticks can never be one chunk, so nextChunk fails before ever
// allocating it, keeping the largest buffer it returns bounded by hardCap.
func nextChunk(f *os.File, base, scanFrom, size int64, agent string, settled bool) (chunk []byte, scannedTo int64, err error) {
	if scanFrom < base {
		scanFrom = base
	}
	window := int64(chunkTarget)
	for {
		end := scanFrom + window
		if end > size {
			end = size
		}
		buf := make([]byte, end-scanFrom)
		if err := readAt(f, buf, scanFrom); err != nil {
			return nil, scanFrom, err
		}
		atEOF := end >= size

		if nl := bytes.LastIndexByte(buf, '\n'); nl >= 0 {
			completeEnd := scanFrom + int64(nl) + 1
			if boundary := boundaryWithin(buf[:nl+1], scanFrom, agent); boundary > 0 {
				if boundary-base > hardCap {
					return nil, scanFrom, errMessageTooBig(agent)
				}
				up, err := readRange(f, base, boundary)
				return up, boundary, err
			}
			// Complete lines, but none closes a message: a Codex turn still open. Once
			// settled, flush it whole; otherwise withhold and resume past these lines.
			if atEOF {
				if completeEnd-base > hardCap {
					return nil, scanFrom, errMessageTooBig(agent)
				}
				if settled {
					up, err := readRange(f, base, completeEnd)
					return up, completeEnd, err
				}
				return nil, completeEnd, nil
			}
		} else if atEOF {
			// No complete line in the scanned tail: an unfinished trailing line.
			if size-base > hardCap {
				return nil, scanFrom, errMessageTooBig(agent)
			}
			return nil, end, nil
		}

		if window >= hardCap {
			return nil, scanFrom, errMessageTooBig(agent)
		}
		window *= 2
		if window > hardCap {
			window = hardCap
		}
	}
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

// readRange reads [from, to) from f into a fresh buffer.
func readRange(f *os.File, from, to int64) ([]byte, error) {
	b := make([]byte, to-from)
	if err := readAt(f, b, from); err != nil {
		return nil, err
	}
	return b, nil
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

// agentCodex is the agent string whose sessions fold a turn across lines. Kept
// local so the upload package does not depend on the parser.
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

// verifyPrefix reports whether the local file's first serverBytes bytes hash to
// the server's prefix hash, and advances fs to record that verification. It avoids
// rehashing the whole prefix on every sync: an append-only file whose prefix was
// already hashed is confirmed by comparing the cached digest (no I/O), and a file
// the server has grown past our cache is confirmed by hashing only the new bytes.
// Only a cold cache, a server rewind, or a truncation forces a full re-hash.
//
// The cheap paths trust that bytes the cache already covered have not changed
// underneath us. That holds for append-only session logs; a same-length in-place
// rewrite of historical bytes would be missed, which is why a shorter file (the
// real-world divergence signal, from rotation or truncation) drops to the full
// re-hash, and the server's per-chunk offset checks backstop the rest.
func (c *Client) verifyPrefix(f *os.File, fs *fileSync, serverBytes, size int64, want string) (bool, error) {
	if size < serverBytes {
		return false, nil // local file is shorter than the server: cannot match
	}
	switch {
	case fs.prefixHasher != nil && fs.base == serverBytes && size >= fs.prefixSize:
		// Append-only growth with the prefix already hashed: compare the cached digest.
		fs.prefixSize = size
		return hex.EncodeToString(fs.prefixHasher.Sum(nil)) == want, nil

	case fs.prefixHasher != nil && fs.base < serverBytes && size >= fs.prefixSize:
		// The server gained bytes since we last verified: extend the digest over only
		// the new span. On a mismatch the caller resets and rebuilds from zero.
		if err := hashRange(f, fs.prefixHasher, fs.base, serverBytes); err != nil {
			return false, err
		}
		if hex.EncodeToString(fs.prefixHasher.Sum(nil)) != want {
			return false, nil
		}
		fs.base = serverBytes
		fs.prefixSize = size
		return true, nil

	default:
		// Cold cache, server rewind, or truncation: re-hash the prefix from zero.
		h := sha256.New()
		if err := hashRange(f, h, 0, serverBytes); err != nil {
			return false, err
		}
		if hex.EncodeToString(h.Sum(nil)) != want {
			return false, nil
		}
		fs.prefixHasher = h
		fs.base = serverBytes
		fs.prefixSize = size
		fs.scanned = serverBytes
		return true, nil
	}
}

// hashRange feeds [from, to) of f into h. A short read (the file was truncated
// since Stat) is an error, not a silently shorter prefix that would hash wrong.
func hashRange(f *os.File, h hash.Hash, from, to int64) error {
	if to <= from {
		return nil
	}
	n, err := io.Copy(h, io.NewSectionReader(f, from, to-from))
	if err != nil {
		return fmt.Errorf("hash session file [%d,%d): %w", from, to, err)
	}
	if n != to-from {
		return fmt.Errorf("hash session file [%d,%d): short read (%d of %d bytes)", from, to, n, to-from)
	}
	return nil
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
