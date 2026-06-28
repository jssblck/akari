package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"

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
func writeBlobTx(ctx context.Context, tx pgx.Tx, content []byte, mediaType string) (string, error) {
	sum := HashBytes(content)

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
	if _, err := lo.Write(content); err != nil {
		_ = lo.Close()
		return "", err
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
func (s *Store) SweepBlobs(ctx context.Context) (int, error) {
	var removed int
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
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
