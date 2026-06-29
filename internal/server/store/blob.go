package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5"
)

// Blob is a stored content-addressed body: its hash, size, and media type. The
// bytes themselves live in a Postgres large object referenced by lo_oid.
type Blob struct {
	SHA256    string
	ByteLen   int64
	MediaType string
}

// HashBytes returns the lowercase hex sha256 of content, the key the CAS uses.
func HashBytes(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// HashString returns the lowercase hex sha256 of content. It hashes in place
// (the digest consumes the string in 64-byte blocks), so a large body is never
// copied into a byte slice just to be hashed.
func HashString(content string) string {
	h := sha256.New()
	_, _ = io.WriteString(h, content)
	return hex.EncodeToString(h.Sum(nil))
}

// blobWriteChunk bounds how much of a body is turned into bytes at once when
// streaming it into a large object. A tool body can be large (a big file read,
// or one oversized turn), so writing it in slices keeps at most one chunk
// resident beyond the source string rather than a full second copy.
const blobWriteChunk = 4 << 20

// writeBlobTx stores content in the CAS within an existing transaction, deduped
// by sha256, and returns its hash. If the hash already exists the content is not
// rewritten. Large objects can only be created inside a transaction, so this is
// always called from one (the projection write).
//
// When the blob already exists we take a FOR KEY SHARE lock on its row before
// returning. That lock conflicts with the FOR UPDATE the sweep takes, so a
// concurrent sweep cannot delete a blob this writer is about to reference: it
// either skips the locked row, or (if it locked first) we see the row gone and
// recreate it. Without the lock the sweep could delete the blob between our check
// and the referencing tool_calls insert, failing the FK.
//
// A concurrent transaction can still insert the same hash between our check and
// our insert; ON CONFLICT DO NOTHING absorbs that, and we unlink the large object
// we created so the loser does not strand one.
func writeBlobTx(ctx context.Context, tx pgx.Tx, content string, mediaType string) (string, error) {
	sum := HashString(content)

	var dummy int
	err := tx.QueryRow(ctx, "SELECT 1 FROM blobs WHERE sha256 = $1 FOR KEY SHARE", sum).Scan(&dummy)
	if err == nil {
		return sum, nil // already present and now locked against a concurrent sweep
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}

	los := tx.LargeObjects()
	oid, err := los.Create(ctx, 0) // 0 lets Postgres assign the oid
	if err != nil {
		return "", err
	}
	lo, err := los.Open(ctx, oid, pgx.LargeObjectModeWrite)
	if err != nil {
		return "", err
	}
	// Write in bounded slices so the body is not duplicated whole: each iteration
	// materializes at most blobWriteChunk bytes, never the full body a second time.
	for i := 0; i < len(content); i += blobWriteChunk {
		j := i + blobWriteChunk
		if j > len(content) {
			j = len(content)
		}
		if _, err := lo.Write([]byte(content[i:j])); err != nil {
			_ = lo.Close()
			return "", err
		}
	}
	if err := lo.Close(); err != nil {
		return "", err
	}

	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	tag, err := tx.Exec(ctx,
		`INSERT INTO blobs (sha256, lo_oid, byte_len, media_type)
		 VALUES ($1, $2, $3, $4) ON CONFLICT (sha256) DO NOTHING`,
		sum, oid, len(content), mediaType)
	if err != nil {
		return "", err
	}
	if tag.RowsAffected() == 0 {
		// Another transaction won the race for this hash; drop the duplicate large
		// object we just created so it does not leak.
		if err := los.Unlink(ctx, oid); err != nil {
			return "", err
		}
	}
	return sum, nil
}

// ErrBlobNotUploaded reports that a tool body the transcript references by sha256
// is not in the CAS. Under the client-CAS protocol the client uploads every body
// before the transcript that references it, so this means an out-of-order or
// dropped upload; the parse leaves the cursor for a retry rather than inventing a
// dangling reference.
var ErrBlobNotUploaded = errors.New("referenced tool body is not present in the CAS")

// pinBlobRefTx locks an already-present blob FOR KEY SHARE so a concurrent sweep
// cannot reclaim it between this check and the referencing tool_calls insert in
// the same transaction. It is the read-side analogue of the lock writeBlobTx
// takes when it finds the hash already present: the sweep's FOR UPDATE conflicts
// with this lock, so the blob survives until the reference commits. A missing
// blob is ErrBlobNotUploaded: the client uploads bodies before the transcript, so
// the row must exist by the time the reference is recorded.
func pinBlobRefTx(ctx context.Context, tx pgx.Tx, sha string) error {
	var dummy int
	err := tx.QueryRow(ctx, "SELECT 1 FROM blobs WHERE sha256 = $1 FOR KEY SHARE", sha).Scan(&dummy)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrBlobNotUploaded
	}
	return err
}

// HaveBlobs returns, for a set of candidate hashes, the subset the CAS already
// holds. The client calls this before uploading tool bodies so a body the server
// already has (from this session's earlier sync, or any other session's, since
// the CAS dedupes globally) is never re-sent.
func (s *Store) HaveBlobs(ctx context.Context, shas []string) (map[string]bool, error) {
	have := map[string]bool{}
	if len(shas) == 0 {
		return have, nil
	}
	rows, err := s.Pool.Query(ctx, "SELECT sha256 FROM blobs WHERE sha256 = ANY($1)", shas)
	if err != nil {
		return nil, fmt.Errorf("query present blobs: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var sha string
		if err := rows.Scan(&sha); err != nil {
			return nil, err
		}
		have[sha] = true
	}
	return have, rows.Err()
}

// blobPinTTL is how long a freshly uploaded, not-yet-referenced blob is protected
// from the sweep. The client uploads a body then sends the transcript that
// references it within one sync, far inside this window; the pin only has to
// outlive that gap (and a crash between the two), after which the tool_calls
// reference keeps the blob alive and the pin lapses harmlessly. It is generous so
// a slow or retried sync cannot lose a body out from under its transcript.
const blobPinTTL = time.Hour

// PutBlob stores a content-addressed body uploaded directly by the client and
// pins it against the sweep for blobPinTTL. The body is streamed in from r in
// bounded slices so neither the client nor the server holds the whole body in
// memory: a 500 MiB tool result lands as a large object without a 500 MiB buffer.
// The stored bytes are verified against the claimed sha256, so a corrupt upload
// cannot poison the CAS (a later transcript referencing that hash would serve
// wrong bytes). An already-present body is not rewritten; its pin is refreshed so
// a re-sync still protects it.
func (s *Store) PutBlob(ctx context.Context, sha, mediaType string, r io.Reader) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		// Refresh the pin first, and on an already-present blob take FOR KEY SHARE so
		// a racing sweep cannot delete it before the pin lands.
		var dummy int
		err := tx.QueryRow(ctx, "SELECT 1 FROM blobs WHERE sha256 = $1 FOR KEY SHARE", sha).Scan(&dummy)
		if err == nil {
			// Drain the request body so the upload completes cleanly even though we
			// already have the content (the client need not special-case a 200 here).
			_, _ = io.Copy(io.Discard, r)
			return upsertBlobPin(ctx, tx, sha)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}

		los := tx.LargeObjects()
		oid, err := los.Create(ctx, 0)
		if err != nil {
			return err
		}
		lo, err := los.Open(ctx, oid, pgx.LargeObjectModeWrite)
		if err != nil {
			return err
		}
		h := sha256.New()
		buf := make([]byte, blobWriteChunk)
		var total int64
		for {
			n, rerr := r.Read(buf)
			if n > 0 {
				if _, werr := lo.Write(buf[:n]); werr != nil {
					_ = lo.Close()
					return werr
				}
				h.Write(buf[:n])
				total += int64(n)
			}
			if rerr == io.EOF {
				break
			}
			if rerr != nil {
				_ = lo.Close()
				return rerr
			}
		}
		if err := lo.Close(); err != nil {
			return err
		}

		got := hex.EncodeToString(h.Sum(nil))
		if got != sha {
			// The bytes do not hash to the claimed key; drop the large object and
			// reject so a mismatched body never enters the CAS under a wrong name.
			_ = los.Unlink(ctx, oid)
			return fmt.Errorf("uploaded blob hash %s does not match declared %s", got, sha)
		}
		if mediaType == "" {
			mediaType = "application/octet-stream"
		}
		tag, err := tx.Exec(ctx,
			`INSERT INTO blobs (sha256, lo_oid, byte_len, media_type)
			 VALUES ($1, $2, $3, $4) ON CONFLICT (sha256) DO NOTHING`,
			sha, oid, total, mediaType)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			// Another upload won the race for this hash; drop our duplicate large
			// object rather than strand it.
			if err := los.Unlink(ctx, oid); err != nil {
				return err
			}
		}
		return upsertBlobPin(ctx, tx, sha)
	})
}

// upsertBlobPin records or refreshes a sweep-protection pin for a blob, extending
// its expiry to now + blobPinTTL.
func upsertBlobPin(ctx context.Context, tx pgx.Tx, sha string) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO blob_pins (sha256, expires_at) VALUES ($1, now() + $2)
		 ON CONFLICT (sha256) DO UPDATE SET expires_at = EXCLUDED.expires_at`,
		sha, blobPinTTL)
	if err != nil {
		return fmt.Errorf("pin blob %s: %w", sha, err)
	}
	return nil
}

// BlobMeta returns a blob's size and media type without reading its body.
func (s *Store) BlobMeta(ctx context.Context, sha256hex string) (Blob, error) {
	var b Blob
	b.SHA256 = sha256hex
	err := s.Pool.QueryRow(ctx,
		"SELECT byte_len, media_type FROM blobs WHERE sha256 = $1", sha256hex).
		Scan(&b.ByteLen, &b.MediaType)
	if errors.Is(err, pgx.ErrNoRows) {
		return Blob{}, ErrNotFound
	}
	return b, err
}

// WriteBlobTo streams a blob's bytes to w and returns its media type. Large
// object reads must run in a transaction, so the copy happens inside one.
func (s *Store) WriteBlobTo(ctx context.Context, w io.Writer, sha256hex string) (mediaType string, err error) {
	err = pgx.BeginTxFunc(ctx, s.Pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var oid uint32
		row := tx.QueryRow(ctx, "SELECT lo_oid, media_type FROM blobs WHERE sha256 = $1", sha256hex)
		if err := row.Scan(&oid, &mediaType); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		los := tx.LargeObjects()
		lo, err := los.Open(ctx, oid, pgx.LargeObjectModeRead)
		if err != nil {
			return err
		}
		defer lo.Close()
		_, err = io.Copy(w, lo)
		return err
	})
	return mediaType, err
}

// SessionReferencesBlob reports whether a session points at a blob, through a
// tool call's input or result or through an attachment. Blob serving is gated on
// this so a session can never read a blob it does not reference, even though the
// CAS dedupes content across sessions.
func (s *Store) SessionReferencesBlob(ctx context.Context, sessionID int64, sha256hex string) (bool, error) {
	var ok bool
	err := s.Pool.QueryRow(ctx,
		`SELECT EXISTS (
		   SELECT 1 FROM tool_calls
		    WHERE session_id = $1 AND (input_sha256 = $2 OR result_sha256 = $2)
		   UNION ALL
		   SELECT 1 FROM attachments
		    WHERE session_id = $1 AND sha256 = $2
		 )`, sessionID, sha256hex).Scan(&ok)
	return ok, err
}

// SweepBlobs deletes every blob no live row references, unlinking its large
// object. Liveness is computed, not refcounted, so the sweep is self-healing: it
// is only needed after a delete or re-parse, the only events that can orphan a
// blob. It returns the number of blobs removed.
//
// A freshly uploaded body the client has not yet referenced from a transcript is
// protected by an unexpired pin (see PutBlob): the orphan predicate excludes any
// blob with a live blob_pins row, so the gap between uploading a body and
// uploading the transcript that references it cannot lose the body. Expired pins
// are cleared first so a body whose transcript never arrived is eventually
// reclaimable.
func (s *Store) SweepBlobs(ctx context.Context) (int, error) {
	var removed int
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "DELETE FROM blob_pins WHERE expires_at <= now()"); err != nil {
			return fmt.Errorf("clear expired blob pins: %w", err)
		}
		// FOR UPDATE conflicts with the FOR KEY SHARE a live writer holds on a blob
		// it is about to reference; SKIP LOCKED makes the sweep pass over those
		// rather than block on (or delete) a blob mid-write. The orphan predicate is
		// re-evaluated against committed state as each row is locked.
		rows, err := tx.Query(ctx,
			`SELECT sha256, lo_oid FROM blobs b
			  WHERE NOT EXISTS (
			          SELECT 1 FROM tool_calls t
			           WHERE t.input_sha256 = b.sha256 OR t.result_sha256 = b.sha256)
			    AND NOT EXISTS (
			          SELECT 1 FROM attachments a WHERE a.sha256 = b.sha256)
			    AND NOT EXISTS (
			          SELECT 1 FROM blob_pins p
			           WHERE p.sha256 = b.sha256 AND p.expires_at > now())
			  FOR UPDATE SKIP LOCKED`)
		if err != nil {
			return err
		}
		type orphan struct {
			sha string
			oid uint32
		}
		var orphans []orphan
		for rows.Next() {
			var o orphan
			if err := rows.Scan(&o.sha, &o.oid); err != nil {
				rows.Close()
				return err
			}
			orphans = append(orphans, o)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		los := tx.LargeObjects()
		for _, o := range orphans {
			if err := los.Unlink(ctx, o.oid); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, "DELETE FROM blobs WHERE sha256 = $1", o.sha); err != nil {
				return err
			}
			removed++
		}
		return nil
	})
	return removed, err
}
