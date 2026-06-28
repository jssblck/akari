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
	"io"
	"net/http"
	"os"
	"strings"
)

// chunkTarget is the preferred chunk size. A chunk is trimmed back to its last
// newline so it carries only complete JSONL lines.
const chunkTarget = 4 << 20

// hardCap bounds how far the client will grow a chunk to reach a newline when a
// single JSONL line is larger than chunkTarget. It stays under the server's
// 64 MiB chunk limit.
const hardCap = 60 << 20

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
}

// New builds a Client. baseURL is the server root (trailing slash optional).
func New(httpClient *http.Client, baseURL, token string) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{http: httpClient, baseURL: strings.TrimRight(baseURL, "/"), token: token}
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

	var totalUploaded int64
	sawReset := false
	for attempt := 0; attempt < maxConflictRetries; attempt++ {
		out, conflicted, err := c.syncOnce(ctx, t, f)
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
func (c *Client) syncOnce(ctx context.Context, t Target, f *os.File) (Outcome, bool, error) {
	ann, err := c.announce(ctx, t)
	if err != nil {
		return Outcome{}, false, err
	}
	info, err := f.Stat()
	if err != nil {
		return Outcome{}, false, err
	}
	size := info.Size()

	action := ActionUploaded
	offset := ann.StoredBytes
	if offset > 0 {
		ok, err := prefixMatches(f, offset, size, ann.PrefixSHA256)
		if err != nil {
			return Outcome{}, false, err
		}
		if !ok {
			// The local file was rotated, truncated, or rewritten, or the server
			// diverged: drop the server copy and re-upload from zero.
			if err := c.reset(ctx, ann.SessionID); err != nil {
				return Outcome{}, false, err
			}
			offset = 0
			action = ActionReset
		}
	}

	out := Outcome{StoredBytes: ann.StoredBytes}
	for offset < size {
		chunk, err := nextChunk(f, offset, size)
		if err != nil {
			return out, false, err
		}
		if len(chunk) == 0 {
			break // only an incomplete trailing line remains; wait for it to finish
		}
		res, err := c.chunk(ctx, ann.SessionID, offset, chunk)
		if err != nil {
			return out, false, err
		}
		if res.conflict {
			return out, true, nil // re-announce and re-verify the prefix
		}
		out.UploadedBytes += int64(len(chunk))
		out.StoredBytes = res.storedBytes
		out.MessageCount = res.messageCount
		offset = res.storedBytes
	}

	if out.UploadedBytes == 0 && action != ActionReset {
		action = ActionUpToDate
	}
	out.Action = action
	return out, false, nil
}

// nextChunk returns the bytes to send next: from offset through the last newline
// within a bounded window, so only complete lines are uploaded. It returns an
// empty slice when only an unfinished trailing line remains, and an error when a
// single line exceeds hardCap.
func nextChunk(f *os.File, offset, size int64) ([]byte, error) {
	window := int64(chunkTarget)
	for {
		end := offset + window
		if end > size {
			end = size
		}
		buf := make([]byte, end-offset)
		if _, err := f.ReadAt(buf, offset); err != nil && err != io.EOF {
			return nil, err
		}
		if nl := bytes.LastIndexByte(buf, '\n'); nl >= 0 {
			return buf[:nl+1], nil
		}
		if end >= size {
			return nil, nil // no newline in the tail: an incomplete final line
		}
		if window >= hardCap {
			return nil, fmt.Errorf("session line exceeds %d bytes without a newline", hardCap)
		}
		window *= 2
		if window > hardCap {
			window = hardCap
		}
	}
}

// prefixMatches reports whether the local file's first storedBytes bytes hash to
// the server's prefix hash. A local file shorter than storedBytes cannot match.
func prefixMatches(f *os.File, storedBytes, size int64, want string) (bool, error) {
	if size < storedBytes {
		return false, nil
	}
	h := sha256.New()
	if _, err := io.Copy(h, io.NewSectionReader(f, 0, storedBytes)); err != nil {
		return false, err
	}
	return hex.EncodeToString(h.Sum(nil)) == want, nil
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
