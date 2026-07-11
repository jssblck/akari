package store_test

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"

	"github.com/jssblck/akari/internal/server/storetest"
)

// TestBackfillMessageContentHashesFillsUnbackfilledRows simulates the state a
// real deploy carries across migration 0049: rows already in the table with
// content_sha256/thinking_text_sha256 left at the unbackfilled sentinel value
// (a state no ordinary write can otherwise produce, since the
// messages_content_hashes trigger stamps every insert; the trigger is disabled
// for one statement here purely to construct that legacy fixture).
func TestBackfillMessageContentHashesFillsUnbackfilledRows(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	u, err := st.Register(ctx, "grace", "hash", "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	projectID, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	sessionID := seedSession(t, st, u.ID, projectID, "backfill-src")

	const rowCount = 7
	if _, err := st.Pool.Exec(ctx, "ALTER TABLE messages DISABLE TRIGGER messages_content_hashes"); err != nil {
		t.Fatalf("disable trigger: %v", err)
	}
	for i := 0; i < rowCount; i++ {
		content := fmt.Sprintf("legacy content %d", i)
		thinking := fmt.Sprintf("legacy thinking %d", i)
		if _, err := st.Pool.Exec(ctx,
			`INSERT INTO messages (session_id, ordinal, role, content, thinking_text, has_thinking, content_sha256, thinking_text_sha256)
			 VALUES ($1,$2,'assistant',$3,$4,true,'','')`,
			sessionID, i, content, thinking,
		); err != nil {
			t.Fatalf("insert unbackfilled message %d: %v", i, err)
		}
	}
	if _, err := st.Pool.Exec(ctx, "ALTER TABLE messages ENABLE TRIGGER messages_content_hashes"); err != nil {
		t.Fatalf("enable trigger: %v", err)
	}

	// A row inserted normally (trigger enabled) is already correctly stamped and
	// must not be touched or double-counted by the backfill.
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO messages (session_id, ordinal, role, content) VALUES ($1,$2,'user','already stamped')`,
		sessionID, rowCount,
	); err != nil {
		t.Fatalf("insert pre-stamped message: %v", err)
	}
	var preStampedHash string
	if err := st.Pool.QueryRow(ctx,
		"SELECT content_sha256 FROM messages WHERE session_id = $1 AND ordinal = $2", sessionID, rowCount,
	).Scan(&preStampedHash); err != nil {
		t.Fatalf("read pre-stamped hash: %v", err)
	}
	if preStampedHash == "" {
		t.Fatal("trigger did not stamp the normally-inserted row")
	}

	n, err := st.BackfillMessageContentHashes(ctx)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if n != rowCount {
		t.Fatalf("backfill processed %d row(s), want %d (should not re-touch the pre-stamped row)", n, rowCount)
	}

	for i := 0; i < rowCount; i++ {
		var contentSHA, thinkingSHA string
		if err := st.Pool.QueryRow(ctx,
			"SELECT content_sha256, thinking_text_sha256 FROM messages WHERE session_id = $1 AND ordinal = $2",
			sessionID, i,
		).Scan(&contentSHA, &thinkingSHA); err != nil {
			t.Fatalf("read backfilled row %d: %v", i, err)
		}
		wantContent := fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("legacy content %d", i))))
		wantThinking := fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("legacy thinking %d", i))))
		if contentSHA != wantContent {
			t.Fatalf("row %d content_sha256 = %s, want %s", i, contentSHA, wantContent)
		}
		if thinkingSHA != wantThinking {
			t.Fatalf("row %d thinking_text_sha256 = %s, want %s", i, thinkingSHA, wantThinking)
		}
	}

	// Idempotent: nothing left to do, and re-running is a safe no-op.
	n2, err := st.BackfillMessageContentHashes(ctx)
	if err != nil {
		t.Fatalf("second backfill: %v", err)
	}
	if n2 != 0 {
		t.Fatalf("second backfill processed %d row(s), want 0", n2)
	}
}

// TestMCPMessageResourcesToleratesUnbackfilledHashColumn is the regression for
// the review finding on migration 0049: with the blocking backfill UPDATE
// removed from the migration, an old row can sit at the sentinel value
// indefinitely (until the background pass reaches it). Both halves of the
// resource-link scheme must still work on such a row: MCPMessagesAfter (link
// generation, on a truncated oversized field) must publish a real, resolvable
// digest rather than propagating the empty sentinel, and MessageText (link
// resolution) must accept that digest without depending on the stored column
// ever having been populated. Running the backfill afterward must not change
// the digest a caller already holds.
func TestMCPMessageResourcesToleratesUnbackfilledHashColumn(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	u, err := st.Register(ctx, "grace", "hash", "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	projectID, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	sessionID := seedSession(t, st, u.ID, projectID, "unbackfilled-src")

	content := "oversized content that forces a preview: " + strings.Repeat("c", 5000)
	thinking := "oversized thinking that forces a preview: " + strings.Repeat("t", 5000)
	if _, err := st.Pool.Exec(ctx, "ALTER TABLE messages DISABLE TRIGGER messages_content_hashes"); err != nil {
		t.Fatalf("disable trigger: %v", err)
	}
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO messages (session_id, ordinal, role, content, thinking_text, has_thinking, content_sha256, thinking_text_sha256)
		 VALUES ($1,0,'assistant',$2,$3,true,'','')`,
		sessionID, content, thinking,
	); err != nil {
		t.Fatalf("insert unbackfilled message: %v", err)
	}
	if _, err := st.Pool.Exec(ctx, "ALTER TABLE messages ENABLE TRIGGER messages_content_hashes"); err != nil {
		t.Fatalf("enable trigger: %v", err)
	}
	// content_sha256 is CHAR(64), which Postgres blank-pads on storage: an empty
	// value round-trips as 64 spaces, not "". btrim strips that padding so this
	// diagnostic read matches the sentinel the way the production SQL's own
	// padding-aware comparisons already do.
	var storedHash string
	if err := st.Pool.QueryRow(ctx, "SELECT btrim(content_sha256) FROM messages WHERE session_id = $1 AND ordinal = 0", sessionID).Scan(&storedHash); err != nil {
		t.Fatalf("read stored hash: %v", err)
	}
	if storedHash != "" {
		t.Fatalf("fixture is not actually unbackfilled: content_sha256 = %q", storedHash)
	}

	// A tiny byte budget forces MCPMessagesAfter to treat this row as oversized
	// and publish a preview plus a hash-bound reference, exactly the path that
	// generates a resource link in the MCP layer.
	msgs, _, _, err := st.MCPMessagesAfter(ctx, sessionID, nil, 10, 200, 64)
	if err != nil {
		t.Fatalf("MCPMessagesAfter: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("MCPMessagesAfter returned %d row(s), want 1", len(msgs))
	}
	m := msgs[0]
	if !m.ContentTruncated || !m.ThinkingTextTruncated {
		t.Fatalf("row not truncated as expected: %+v", m)
	}
	wantContentHash := fmt.Sprintf("%x", sha256.Sum256([]byte(content)))
	wantThinkingHash := fmt.Sprintf("%x", sha256.Sum256([]byte(thinking)))
	if m.ContentSHA256 != wantContentHash {
		t.Fatalf("generated content hash = %q, want %q (must not be the unbackfilled sentinel)", m.ContentSHA256, wantContentHash)
	}
	if m.ThinkingTextSHA256 != wantThinkingHash {
		t.Fatalf("generated thinking hash = %q, want %q (must not be the unbackfilled sentinel)", m.ThinkingTextSHA256, wantThinkingHash)
	}

	// Resolution must accept that hash even though the stored column is still ''.
	gotContent, err := st.MessageText(ctx, sessionID, 0, "content", m.ContentSHA256)
	if err != nil {
		t.Fatalf("MessageText content: %v", err)
	}
	if gotContent != content {
		t.Fatalf("resolved content mismatch: got %d bytes, want %d bytes", len(gotContent), len(content))
	}
	gotThinking, err := st.MessageText(ctx, sessionID, 0, "thinking", m.ThinkingTextSHA256)
	if err != nil {
		t.Fatalf("MessageText thinking: %v", err)
	}
	if gotThinking != thinking {
		t.Fatalf("resolved thinking mismatch: got %d bytes, want %d bytes", len(gotThinking), len(thinking))
	}

	// A stale or tampered hash must still be refused.
	if _, err := st.MessageText(ctx, sessionID, 0, "content", wantThinkingHash); err == nil {
		t.Fatal("MessageText accepted a hash that does not match the field")
	}

	// The background backfill must not change the digest a caller already
	// holds, and resolution must keep working once the column catches up.
	if _, err := st.BackfillMessageContentHashes(ctx); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if err := st.Pool.QueryRow(ctx, "SELECT btrim(content_sha256) FROM messages WHERE session_id = $1 AND ordinal = 0", sessionID).Scan(&storedHash); err != nil {
		t.Fatalf("read backfilled hash: %v", err)
	}
	if storedHash != wantContentHash {
		t.Fatalf("backfilled content_sha256 = %q, want %q", storedHash, wantContentHash)
	}
	if gotAfter, err := st.MessageText(ctx, sessionID, 0, "content", m.ContentSHA256); err != nil || gotAfter != content {
		t.Fatalf("resolution broke after backfill: content=%q err=%v", gotAfter, err)
	}
}
