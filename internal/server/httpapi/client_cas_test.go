package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jssblck/akari/internal/client/upload"
	"github.com/jssblck/akari/internal/parser"
	"github.com/jssblck/akari/internal/server/auth"
	"github.com/jssblck/akari/internal/server/store"
	"github.com/klauspost/compress/zstd"
)

// sha256Hex is the lowercase hex sha256 of b, for asserting a stored blob's bytes
// hash to its CAS key.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ingestClient brings up the real upload.Client against the test server with a
// fresh ingest token, plus the owning user id for store assertions.
func ingestClient(t *testing.T, srv string, st *store.Store) (*upload.Client, int64) {
	t.Helper()
	ctx := context.Background()
	owner, err := st.Register(ctx, "grace", mustHash(t, "hopper-1906"), "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	rawToken, err := auth.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateAPIToken(ctx, owner.ID, "laptop", "ingest", auth.HashToken(rawToken)); err != nil {
		t.Fatalf("create token: %v", err)
	}
	return upload.New(nil, srv, rawToken), owner.ID
}

// writeSession writes content to a temp file whose mtime is old enough that the
// uploader treats the file as settled (so a trailing turn is flushed). It returns
// the path.
func writeSession(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-10 * time.Minute)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	return path
}

func casTarget(path string) upload.Target {
	return upload.Target{
		Agent: "claude", Path: path, SourceID: "cas-sess",
		Kind: "remote", ProjectKey: "github.com/jssblck/akari", Machine: "laptop",
	}
}

// TestClientCASRoundTrip drives the whole client-CAS protocol against the real
// server: the client lifts a tool input and result to the CAS, uploads the
// transformed transcript, and the server records references whose bodies serve
// back byte for byte. It is the end-to-end equivalent of the parser round-trip.
func TestClientCASRoundTrip(t *testing.T) {
	srv, st := newTestServer(t)
	c, ownerID := ingestClient(t, srv.URL, st)
	ctx := context.Background()

	input := `{"file_path":"src/auth.ts"}`
	result := "export function login() {}"
	content := `{"type":"user","message":{"content":"fix it"}}` + "\n" +
		`{"type":"assistant","message":{"id":"m1","model":"claude-sonnet-4-20250514","content":[{"type":"tool_use","id":"t1","name":"Read","input":` + input + `}],"usage":{"input_tokens":10,"output_tokens":5}}}` + "\n" +
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"export function login() {}","is_error":false}]}}` + "\n"
	path := writeSession(t, content)

	out, err := c.SyncFile(ctx, casTarget(path))
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if out.Action != upload.ActionUploaded {
		t.Fatalf("action = %s, want uploaded", out.Action)
	}

	// The stored transcript is the TRANSFORMED one: smaller than the original and
	// carrying no inline body.
	sid := sessionID(t, st, ownerID)
	stored := storedRaw(t, st, sid)
	if strings.Contains(stored, input) || strings.Contains(stored, result) {
		t.Fatalf("stored transcript still inlines a tool body:\n%s", stored)
	}
	if !strings.Contains(stored, "__akari_cas__") {
		t.Fatalf("stored transcript carries no sentinel:\n%s", stored)
	}

	// The tool_call references the bodies, and they serve back exactly.
	var inputSHA, resultSHA string
	if err := st.Pool.QueryRow(ctx,
		"SELECT input_sha256, result_sha256 FROM tool_calls WHERE session_id=$1", sid).Scan(&inputSHA, &resultSHA); err != nil {
		t.Fatal(err)
	}
	assertBlob(t, st, inputSHA, input, "application/json")
	assertBlob(t, st, resultSHA, result, "text/plain")
}

// TestBlobUploadRejectsUnknownContentType confirms the upload handler refuses a
// content_type it cannot label on the way back out, fail-closed, rather than storing a
// body it could not serve with a correct Content-Encoding. Only raw and zstd are
// accepted; anything else is a 400 and nothing is stored. The same body with a valid
// content_type is accepted and persists that content_type, so the rejection is the
// label and not the request shape.
func TestBlobUploadRejectsUnknownContentType(t *testing.T) {
	srv, st := newTestServer(t)
	ctx := context.Background()
	owner, err := st.Register(ctx, "ada", mustHash(t, "lovelace-1843"), "")
	if err != nil {
		t.Fatal(err)
	}
	rawToken, err := auth.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateAPIToken(ctx, owner.ID, "laptop", "ingest", auth.HashToken(rawToken)); err != nil {
		t.Fatal(err)
	}

	body := []byte("a tool body the server stores opaquely")
	sha := store.HashBytes(body)
	put := func(ct string) *http.Response {
		u := fmt.Sprintf("%s/api/v1/ingest/blob/%s?media_type=text/plain&content_type=%s", srv.URL, sha, url.QueryEscape(ct))
		req, err := http.NewRequest(http.MethodPut, u, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+rawToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("put blob: %v", err)
		}
		return resp
	}

	resp := put("application/gzip")
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown content_type status = %d, want 400", resp.StatusCode)
	}
	if _, err := st.BlobMeta(ctx, sha); err == nil {
		t.Fatal("a rejected upload must not be stored")
	}

	// The bytes are labeled zstd but are not actually a zstd frame; the server stores
	// them verbatim and never decompresses, so the hash match alone admits them.
	resp = put(parser.ContentZstd)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("valid content_type status = %d, want 200", resp.StatusCode)
	}
	meta, err := st.BlobMeta(ctx, sha)
	if err != nil || meta.ContentType != parser.ContentZstd {
		t.Fatalf("stored content_type = %q (err=%v), want %q", meta.ContentType, err, parser.ContentZstd)
	}
}

// TestClientCASDedupOnResync is the no-churn invariant: syncing an unchanged file
// a second time uploads zero transcript bytes and zero bodies. It proves the
// transform is byte stable and the CAS dedup short-circuits the body upload.
func TestClientCASDedupOnResync(t *testing.T) {
	srv, st := newTestServer(t)
	c, _ := ingestClient(t, srv.URL, st)
	ctx := context.Background()

	content := `{"type":"user","message":{"content":"hi"}}` + "\n" +
		`{"type":"assistant","message":{"id":"m1","model":"claude-sonnet-4-20250514","content":[{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"a.go"}}]}}` + "\n" +
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"package a","is_error":false}]}}` + "\n"
	path := writeSession(t, content)

	if _, err := c.SyncFile(ctx, casTarget(path)); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	out, err := c.SyncFile(ctx, casTarget(path))
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if out.Action != upload.ActionUpToDate {
		t.Fatalf("second sync action = %s, want uptodate", out.Action)
	}
	if out.UploadedBytes != 0 {
		t.Fatalf("second sync uploaded %d bytes, want 0", out.UploadedBytes)
	}

	// A fresh client (cold cache) re-syncing the same file also moves nothing: the
	// cold-path re-transform verifies the prefix and finds it complete.
	cold := upload.New(nil, srv.URL, tokenFor(t, st))
	out, err = cold.SyncFile(ctx, casTarget(path))
	if err != nil {
		t.Fatalf("cold sync: %v", err)
	}
	if out.Action != upload.ActionUpToDate || out.UploadedBytes != 0 {
		t.Fatalf("cold re-sync action=%s uploaded=%d, want uptodate/0", out.Action, out.UploadedBytes)
	}
}

// TestClientCASBigBody is the regression test for the 508 MiB turn: a tool result
// far larger than the chunk cap uploads successfully because the transcript stays
// tiny (a sentinel) and the body streams to the CAS as its own upload.
func TestClientCASBigBody(t *testing.T) {
	srv, st := newTestServer(t)
	c, ownerID := ingestClient(t, srv.URL, st)
	ctx := context.Background()

	// A result well past the server's 128 MiB maxChunk would be impossible to
	// upload inline. Use a size comfortably over the cap to prove the body no longer
	// rides in the transcript. Kept to ~160 MiB so the test stays quick.
	big := strings.Repeat("Z", 160<<20)
	content := `{"type":"user","message":{"content":"dump it"}}` + "\n" +
		`{"type":"assistant","message":{"id":"m1","model":"claude-sonnet-4-20250514","content":[{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"big"}}]}}` + "\n" +
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"` + big + `","is_error":false}]}}` + "\n"
	path := writeSession(t, content)

	out, err := c.SyncFile(ctx, casTarget(path))
	if err != nil {
		t.Fatalf("sync big body: %v", err)
	}
	if out.Action != upload.ActionUploaded {
		t.Fatalf("action = %s, want uploaded", out.Action)
	}

	sid := sessionID(t, st, ownerID)
	stored := storedRaw(t, st, sid)
	if int64(len(stored)) > maxChunk {
		t.Fatalf("stored transcript is %d bytes, larger than maxChunk: the body did not move to the CAS", len(stored))
	}
	var resultSHA string
	var resultBytes int64
	if err := st.Pool.QueryRow(ctx,
		"SELECT result_sha256, result_bytes FROM tool_calls WHERE session_id=$1", sid).Scan(&resultSHA, &resultBytes); err != nil {
		t.Fatal(err)
	}
	if resultBytes != int64(len(big)) {
		t.Fatalf("result_bytes = %d, want %d (the raw body length)", resultBytes, len(big))
	}
	// The body is far past the compression threshold, so the CAS stores it zstd and
	// keys it by the hash of the compressed bytes, not the raw body. The stored blob's
	// content type must say zstd, and decompressing its bytes must reproduce the body.
	meta, err := st.BlobMeta(ctx, resultSHA)
	if err != nil {
		t.Fatalf("blob meta: %v", err)
	}
	if meta.ContentType != parser.ContentZstd {
		t.Fatalf("big result content_type = %q, want %q", meta.ContentType, parser.ContentZstd)
	}
	var blobBytes bytes.Buffer
	if _, err := st.WriteBlobTo(ctx, &blobBytes, resultSHA); err != nil {
		t.Fatalf("read stored blob: %v", err)
	}
	if got := sha256Hex(blobBytes.Bytes()); got != resultSHA {
		t.Fatalf("stored bytes do not hash to the key: got %s, key %s", got, resultSHA)
	}
	dec, err := zstd.NewReader(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	raw, err := dec.DecodeAll(blobBytes.Bytes(), nil)
	if err != nil {
		t.Fatalf("decompress stored blob: %v", err)
	}
	if len(raw) != len(big) || string(raw) != big {
		t.Fatalf("decompressed body mismatch: got %d bytes, want %d", len(raw), len(big))
	}
}

// TestClientCASResume is the resume invariant: an upload interrupted partway
// (server already holds the first turn's transformed bytes) resumes from the
// server's cursor and lands the same final transcript and references.
func TestClientCASResume(t *testing.T) {
	srv, st := newTestServer(t)
	c, ownerID := ingestClient(t, srv.URL, st)
	ctx := context.Background()

	line1 := `{"type":"user","message":{"content":"first"}}` + "\n"
	full := line1 +
		`{"type":"assistant","message":{"id":"m1","model":"claude-sonnet-4-20250514","content":[{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"a.go"}}]}}` + "\n" +
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"package a","is_error":false}]}}` + "\n"

	// First sync only the opening line (a partial file), then grow it and sync the
	// rest: the second sync must resume, not re-upload from zero.
	path := writeSession(t, line1)
	if _, err := c.SyncFile(ctx, casTarget(path)); err != nil {
		t.Fatalf("partial sync: %v", err)
	}
	storedAfterFirst := int64(len(storedRaw(t, st, sessionID(t, st, ownerID))))

	writeAppend(t, path, full)
	out, err := c.SyncFile(ctx, casTarget(path))
	if err != nil {
		t.Fatalf("resume sync: %v", err)
	}
	if out.Action != upload.ActionUploaded {
		t.Fatalf("resume action = %s, want uploaded (not reset)", out.Action)
	}

	sid := sessionID(t, st, ownerID)
	stored := storedRaw(t, st, sid)
	if !strings.HasPrefix(stored, line1) {
		t.Fatalf("resumed transcript does not start with the already-stored line")
	}
	if int64(len(stored)) <= storedAfterFirst {
		t.Fatalf("resume appended nothing: stored grew from %d to %d", storedAfterFirst, len(stored))
	}
	// The tool call landed with its result reference, proving the appended turns
	// parsed on top of the resumed prefix.
	var resultSHA string
	if err := st.Pool.QueryRow(ctx, "SELECT result_sha256 FROM tool_calls WHERE session_id=$1", sid).Scan(&resultSHA); err != nil {
		t.Fatal(err)
	}
	assertBlob(t, st, resultSHA, "package a", "text/plain")
}

// TestBlobServeContentEncoding is the serve-side contract: a compressed blob is
// handed back with Content-Encoding: zstd and the semantic media type, so a browser
// decodes it transparently while the server streams the stored bytes untouched; a
// small uncompressed blob carries no Content-Encoding and serves verbatim. The
// server itself never (de)compresses on either path.
func TestBlobServeContentEncoding(t *testing.T) {
	srv, st := newTestServer(t)
	c, ownerID := ingestClient(t, srv.URL, st)
	ctx := context.Background()

	// A small input (stored raw) and a large, highly compressible result (stored
	// zstd) in one session.
	smallInput := `{"file_path":"src/auth.ts"}`
	bigResult := strings.Repeat("compress me ", 4096) // ~48 KiB, far over the threshold
	content := `{"type":"user","message":{"content":"go"}}` + "\n" +
		`{"type":"assistant","message":{"id":"m1","model":"claude-sonnet-4-20250514","content":[{"type":"tool_use","id":"t1","name":"Read","input":` + smallInput + `}]}}` + "\n" +
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":` + jsonStringLiteral(bigResult) + `,"is_error":false}]}}` + "\n"
	if _, err := c.SyncFile(ctx, casTarget(writeSession(t, content))); err != nil {
		t.Fatalf("sync: %v", err)
	}

	sid := sessionID(t, st, ownerID)
	var inputSHA, resultSHA string
	if err := st.Pool.QueryRow(ctx,
		"SELECT input_sha256, result_sha256 FROM tool_calls WHERE session_id=$1", sid).Scan(&inputSHA, &resultSHA); err != nil {
		t.Fatal(err)
	}

	// Log in as the owner so the authenticated blob route is reachable.
	web := newClient(t)
	if _, err := web.PostForm(srv.URL+"/login", url.Values{
		"username": {"grace"}, "password": {"hopper-1906"},
	}); err != nil {
		t.Fatalf("login: %v", err)
	}
	// The Go HTTP client only auto-decodes gzip, never zstd, and it did not request
	// zstd, so the response body is the stored bytes verbatim: exactly what this test
	// wants to inspect.
	getRaw := func(sha string) (*http.Response, []byte) {
		resp, err := web.Get(fmt.Sprintf("%s/api/v1/session/%d/blob/%s", srv.URL, sid, sha))
		if err != nil {
			t.Fatalf("get blob %s: %v", sha, err)
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read blob %s: %v", sha, err)
		}
		return resp, raw
	}

	// The large result is served compressed: Content-Encoding zstd, semantic media
	// type text/plain, and the bytes decode back to the original.
	resp, gotBig := getRaw(resultSHA)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("big blob status = %d", resp.StatusCode)
	}
	if enc := resp.Header.Get("Content-Encoding"); enc != "zstd" {
		t.Fatalf("big blob Content-Encoding = %q, want zstd", enc)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("big blob Content-Type = %q, want text/plain", ct)
	}
	dec, err := zstd.NewReader(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	rawBig, err := dec.DecodeAll(gotBig, nil)
	if err != nil {
		t.Fatalf("decompress served big blob: %v", err)
	}
	if string(rawBig) != bigResult {
		t.Fatalf("served big blob decoded to %d bytes, want %d", len(rawBig), len(bigResult))
	}

	// The small input is served verbatim with no Content-Encoding.
	resp, gotSmall := getRaw(inputSHA)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("small blob status = %d", resp.StatusCode)
	}
	if enc := resp.Header.Get("Content-Encoding"); enc != "" {
		t.Fatalf("small blob Content-Encoding = %q, want none", enc)
	}
	if string(gotSmall) != smallInput {
		t.Fatalf("small blob body = %q, want %q", gotSmall, smallInput)
	}
}

// jsonStringLiteral renders s as a JSON string literal for embedding a large body in
// a transcript fixture line.
func jsonStringLiteral(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// --- helpers ---

func tokenFor(t *testing.T, st *store.Store) string {
	t.Helper()
	ctx := context.Background()
	uid := sessionUserID(t, st)
	raw, err := auth.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateAPIToken(ctx, uid, "second", "ingest", auth.HashToken(raw)); err != nil {
		t.Fatal(err)
	}
	return raw
}

func sessionUserID(t *testing.T, st *store.Store) int64 {
	t.Helper()
	var uid int64
	if err := st.Pool.QueryRow(context.Background(), "SELECT id FROM users ORDER BY id LIMIT 1").Scan(&uid); err != nil {
		t.Fatal(err)
	}
	return uid
}

func sessionID(t *testing.T, st *store.Store, ownerID int64) int64 {
	t.Helper()
	var id int64
	if err := st.Pool.QueryRow(context.Background(),
		"SELECT id FROM sessions WHERE user_id=$1 ORDER BY id DESC LIMIT 1", ownerID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func storedRaw(t *testing.T, st *store.Store, sid int64) string {
	t.Helper()
	rows, err := st.Pool.Query(context.Background(),
		"SELECT content FROM session_raw_chunks WHERE session_id=$1 ORDER BY byte_offset", sid)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []byte
	for rows.Next() {
		var c []byte
		if err := rows.Scan(&c); err != nil {
			t.Fatal(err)
		}
		out = append(out, c...)
	}
	return string(out)
}

func assertBlob(t *testing.T, st *store.Store, sha, wantBody, wantMedia string) {
	t.Helper()
	if sha == "" {
		t.Fatalf("expected a blob reference, got empty sha")
	}
	var buf strings.Builder
	media, err := st.WriteBlobTo(context.Background(), &buf, sha)
	if err != nil {
		t.Fatalf("read blob %s: %v", sha, err)
	}
	if buf.String() != wantBody {
		t.Fatalf("blob %s body = %q, want %q", sha, buf.String(), wantBody)
	}
	if media != wantMedia {
		t.Fatalf("blob %s media = %q, want %q", sha, media, wantMedia)
	}
}

func writeAppend(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-10 * time.Minute)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
}
