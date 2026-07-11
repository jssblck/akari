package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

// Blob is a stored content-addressed body: its key, stored size, semantic media
// type, and storage content type. The bytes themselves live in a Postgres large
// object referenced by lo_oid and are stored exactly as the client uploaded them.
// ContentType names how those bytes are encoded (application/octet-stream verbatim,
// or application/zstd compressed): the server never (de)compresses, so it serves
// this as the response's Content-Encoding and lets the client decode. ByteLen is the
// stored (possibly compressed) size; the raw body size lives on the tool_calls row.
type Blob struct {
	SHA256      string
	ByteLen     int64
	MediaType   string
	ContentType string
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
	if err := storeNewBlobTx(ctx, tx, sum, content, mediaType); err != nil {
		return "", err
	}
	return sum, nil
}

// storeNewBlobTx streams content into a new large object and inserts its blobs row,
// for a hash an existence check just reported absent. A concurrent transaction can
// still insert the same hash between that check and this insert; ON CONFLICT DO
// NOTHING absorbs the race, and the loser unlinks the large object it created so a
// duplicate is never stranded.
func storeNewBlobTx(ctx context.Context, tx pgx.Tx, sum, content, mediaType string) error {
	los := tx.LargeObjects()
	oid, err := los.Create(ctx, 0) // 0 lets Postgres assign the oid
	if err != nil {
		return err
	}
	lo, err := los.Open(ctx, oid, pgx.LargeObjectModeWrite)
	if err != nil {
		return err
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
			return err
		}
	}
	if err := lo.Close(); err != nil {
		return err
	}

	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	tag, err := tx.Exec(ctx,
		`INSERT INTO blobs (sha256, lo_oid, byte_len, media_type)
		 VALUES ($1, $2, $3, $4) ON CONFLICT (sha256) DO NOTHING`,
		sum, oid, len(content), mediaType)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		// Another transaction won the race for this hash; drop the duplicate large
		// object we just created so it does not leak.
		if err := los.Unlink(ctx, oid); err != nil {
			return err
		}
	}
	return nil
}

// ErrBlobNotUploaded reports that a tool body the transcript references by sha256
// is not in the CAS. Under the client-CAS protocol the client uploads every body
// before the transcript that references it, so this means an out-of-order or
// dropped upload; the parse leaves the cursor for a retry rather than inventing a
// dangling reference.
var ErrBlobNotUploaded = errors.New("referenced tool body is not present in the CAS")

// blobRef is one client-lifted blob reference a projection write must resolve: the
// hash, plus the error context a missing blob reports under. localWrite marks a
// reference collected after an inline write of the same hash in the same batch,
// which the write order inside the transaction satisfies by itself.
type blobRef struct {
	sha        string
	localWrite bool
	errCtx     string
}

// blobWrite is one inline body a projection write must store, pre-hashed so the
// batched existence check covers it alongside the references.
type blobWrite struct {
	sha    string
	body   string
	media  string
	errCtx string
}

// resolveBlobsTx does a projection write's blob work in bulk: one check-and-lock
// query over every hash the write touches (client-lifted references and inline
// bodies alike), then a large-object write for only the truly new content. A
// per-body probe would cost a rebuild one sequential round trip per tool body,
// hundreds for a tool-heavy session and corpus-wide on an epoch bump; the batch
// costs one query regardless of body count.
//
// The SELECT takes FOR KEY SHARE on every present row, so a concurrent sweep (whose
// FOR UPDATE conflicts and skips locked rows) cannot reclaim a blob between this
// check and the referencing insert in the same transaction. A reference to a hash
// the CAS does not hold is ErrBlobNotUploaded: the client uploads bodies before the
// transcript, so the row must exist by the time the reference is recorded.
func resolveBlobsTx(ctx context.Context, tx pgx.Tx, refs []blobRef, writes []blobWrite) error {
	if len(refs) == 0 && len(writes) == 0 {
		return nil
	}
	shas := make([]string, 0, len(refs)+len(writes))
	for _, r := range refs {
		shas = append(shas, r.sha)
	}
	for _, w := range writes {
		shas = append(shas, w.sha)
	}
	rows, err := tx.Query(ctx, "SELECT sha256 FROM blobs WHERE sha256 = ANY($1) FOR KEY SHARE", shas)
	if err != nil {
		return fmt.Errorf("lock %d referenced blobs: %w", len(shas), err)
	}
	present := make(map[string]bool, len(shas))
	for rows.Next() {
		var sha string
		if err := rows.Scan(&sha); err != nil {
			rows.Close()
			return fmt.Errorf("iterate locked blob hashes: %w", err)
		}
		present[sha] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate locked blob hashes: %w", err)
	}
	for _, r := range refs {
		if !present[r.sha] && !r.localWrite {
			return fmt.Errorf("%s: %w", r.errCtx, ErrBlobNotUploaded)
		}
	}
	// Store genuinely new content in sha order, the same global order every
	// pinner locks in. Two concurrent projection writes carrying the same new
	// bodies would otherwise insert them in each transcript's own order and can
	// deadlock on the blobs unique index, each waiting on the other's uncommitted
	// insert of the sha it wants next.
	sort.Slice(writes, func(i, j int) bool { return writes[i].sha < writes[j].sha })
	for _, w := range writes {
		if present[w.sha] {
			continue // already stored, and the batch SELECT locked it against the sweep
		}
		if err := storeNewBlobTx(ctx, tx, w.sha, w.body, w.media); err != nil {
			return fmt.Errorf("%s: %w", w.errCtx, err)
		}
	}
	return nil
}

// MissingBlobs reports which of a set of candidate hashes the CAS does not hold,
// and atomically (re)pins every hash it does hold. The client calls this before
// uploading tool bodies: a body the server already has (from an earlier sync, or
// any other session, since the CAS dedupes globally) is reported absent from the
// missing set and so not re-sent, but it is pinned here so it survives the sweep
// until the transcript chunk that references it commits. Without the pin a present
// but unreferenced, unpinned blob could be reclaimed in the window between this
// check and the transcript append, stranding a sentinel with no body.
//
// The whole check-and-pin runs in one transaction so the pin is durable before
// the client is told a body is present. Pinning takes the blob rows FOR KEY SHARE
// (via the upsert's FK validation) which conflicts with the sweep's FOR UPDATE, so
// a body cannot be both reported-present and swept.
func (s *Store) MissingBlobs(ctx context.Context, shas []string) ([]string, error) {
	missing := []string{}
	if len(shas) == 0 {
		return missing, nil
	}
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, "SELECT sha256 FROM blobs WHERE sha256 = ANY($1)", shas)
		if err != nil {
			return fmt.Errorf("scan present blobs among %d candidates: %w", len(shas), err)
		}
		present := map[string]bool{}
		for rows.Next() {
			var sha string
			if err := rows.Scan(&sha); err != nil {
				rows.Close()
				return fmt.Errorf("iterate present blob hashes: %w", err)
			}
			present[sha] = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate present blob hashes: %w", err)
		}
		// Partition into present and missing. Pin the present hashes in a deterministic
		// (sorted, deduped) order: two concurrent checks with overlapping hashes in
		// opposite request order would otherwise each lock one pin row and then wait on
		// the other, a classic two-row deadlock that Postgres resolves by aborting one.
		// Locking in sorted order means any two transactions take the rows in the same
		// sequence, so they queue instead of deadlocking.
		var toPin []string
		for _, sha := range shas {
			if present[sha] {
				toPin = append(toPin, sha)
			} else {
				missing = append(missing, sha)
			}
		}
		sort.Strings(toPin)
		var lastPinned string
		for _, sha := range toPin {
			if sha == lastPinned {
				continue // a duplicate hash in the request: pin once
			}
			if err := upsertBlobPin(ctx, tx, sha); err != nil {
				return err
			}
			lastPinned = sha
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return missing, nil
}

// blobPinTTL is how long a freshly uploaded, not-yet-referenced blob is protected
// from the sweep. The client uploads a body then sends the transcript that
// references it within one sync, far inside this window; the pin only has to
// outlive that gap (and a crash between the two), after which the tool_calls
// reference keeps the blob alive and the pin lapses harmlessly. It is generous so
// a slow or retried sync cannot lose a body out from under its transcript.
const blobPinTTL = time.Hour

// ErrBlobHashMismatch reports that the uploaded bytes did not hash to the declared
// key, so the body was not stored. The handler maps it to a 400: it is the
// client's error, not a server fault.
var ErrBlobHashMismatch = errors.New("uploaded blob bytes do not match the declared hash")

// PutBlob stores a content-addressed body uploaded directly by the client and
// pins it against the sweep for blobPinTTL. The bytes the client sends are the
// STORED bytes (raw or zstd-compressed, as contentType declares); the server stores
// them verbatim and never (de)compresses, so it stays off the compression CPU path.
// The body streams in from r in bounded slices so neither side holds the whole body
// in memory: a 500 MiB tool result lands as a large object without a 500 MiB buffer.
// The stored bytes are verified against the claimed sha256 (which is the hash of the
// stored bytes), so a corrupt upload cannot poison the CAS; the server does not
// validate that a zstd-declared body actually decompresses, since that would cost the
// CPU this design avoids and the key already pins the exact bytes.
//
// No database lock is held across the network read. An already-present body is
// pinned and committed in a short transaction before its (redundant) body is
// drained, so a slow duplicate upload cannot block the sweep's FOR UPDATE behind a
// FOR KEY SHARE held across a client-controlled read. A new body is written inside
// one transaction (Postgres large objects require it), but that transaction holds
// no lock on any existing row until it inserts the new blobs row at the end, so it
// does not block the sweep either.
func (s *Store) PutBlob(ctx context.Context, sha, mediaType, contentType string, r io.Reader) error {
	// Fast path: if the blob is already present, pin it and commit before touching
	// the network, then drain the redundant body outside any transaction.
	present, err := s.pinIfPresent(ctx, sha)
	if err != nil {
		return err
	}
	if present {
		// Drain so the client's PUT completes cleanly; it need not special-case a body
		// the server already has. The drain is a bounded, cancellation-aware loop rather
		// than io.Copy so a canceled request (a shutdown, or the client hanging up) stops
		// reading a large redundant body instead of running to its end. The body is
		// discarded, so its bytes are never held.
		return drainBody(ctx, r)
	}

	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		los := tx.LargeObjects()
		oid, err := los.Create(ctx, 0)
		if err != nil {
			return fmt.Errorf("create large object for blob %s: %w", sha, err)
		}
		lo, err := los.Open(ctx, oid, pgx.LargeObjectModeWrite)
		if err != nil {
			return fmt.Errorf("open large object for blob %s: %w", sha, err)
		}
		h := sha256.New()
		buf := make([]byte, blobWriteChunk)
		var total int64
		for {
			// Stop a canceled upload mid-stream rather than draining a huge body.
			if err := ctx.Err(); err != nil {
				_ = lo.Close()
				return err
			}
			n, rerr := r.Read(buf)
			if n > 0 {
				if _, werr := lo.Write(buf[:n]); werr != nil {
					_ = lo.Close()
					return fmt.Errorf("write large object for blob %s: %w", sha, werr)
				}
				h.Write(buf[:n])
				total += int64(n)
			}
			if rerr == io.EOF {
				break
			}
			if rerr != nil {
				_ = lo.Close()
				return fmt.Errorf("read upload body for blob %s: %w", sha, rerr)
			}
		}
		if err := lo.Close(); err != nil {
			return fmt.Errorf("close large object for blob %s: %w", sha, err)
		}

		if got := hex.EncodeToString(h.Sum(nil)); got != sha {
			// The bytes do not hash to the claimed key; drop the large object and reject
			// so a mismatched body never enters the CAS under a wrong name.
			if err := los.Unlink(ctx, oid); err != nil {
				return fmt.Errorf("unlink mismatched large object for blob %s: %w", sha, err)
			}
			return fmt.Errorf("%w: got %s for declared %s", ErrBlobHashMismatch, got, sha)
		}
		tag, err := tx.Exec(ctx,
			`INSERT INTO blobs (sha256, lo_oid, byte_len, media_type, content_type)
			 VALUES ($1, $2, $3, $4, $5) ON CONFLICT (sha256) DO NOTHING`,
			sha, oid, total, mediaType, contentType)
		if err != nil {
			return fmt.Errorf("insert blob row %s: %w", sha, err)
		}
		if tag.RowsAffected() == 0 {
			// Another upload won the race for this hash; drop our duplicate large object
			// rather than strand it.
			if err := los.Unlink(ctx, oid); err != nil {
				return fmt.Errorf("unlink duplicate large object for blob %s: %w", sha, err)
			}
		}
		return upsertBlobPin(ctx, tx, sha)
	})
}

// drainBody reads and discards r in bounded slices, checking for cancellation
// between them. It lets the server consume a redundant duplicate-upload body (one it
// already holds) so the client's PUT completes, without io.Copy's unbounded,
// uncancellable read. A read error is not fatal: the body is being thrown away, so a
// truncated or reset duplicate upload changes nothing.
func drainBody(ctx context.Context, r io.Reader) error {
	buf := make([]byte, blobWriteChunk)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, err := r.Read(buf)
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return nil // discarding the body, so a read failure here is harmless
		}
	}
}

// pinIfPresent pins a blob and reports whether it exists, in one short
// transaction that holds no lock across any network read. The pin upsert validates
// the blob_pins FK by taking the blob row FOR KEY SHARE, which conflicts with the
// sweep's FOR UPDATE, so a present blob cannot be swept between this check and the
// commit.
func (s *Store) pinIfPresent(ctx context.Context, sha string) (bool, error) {
	var present bool
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var dummy int
		err := tx.QueryRow(ctx, "SELECT 1 FROM blobs WHERE sha256 = $1", sha).Scan(&dummy)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("check blob %s before storing: %w", sha, err)
		}
		present = true
		return upsertBlobPin(ctx, tx, sha)
	})
	return present, err
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

// pinSessionBlobsTx refreshes a sweep-protection pin on every blob a session
// references through its lifted tool bodies and image attachments. The reparse path
// calls it before clearing those rows so the blobs survive the window between the
// clear (committed here) and the rebuild (the following Advance).
//
// The pin runs entirely in the database as one INSERT ... SELECT over the union of
// referenced hashes, so no per-session slice of hashes is held in Go: a session with
// many lifted bodies or images costs the same resident memory as one with a few. The
// UNION dedups a blob referenced by two rows down to a single pin, and the ORDER BY
// fixes the order rows are inserted and so the order their pin rows are locked, so two
// concurrent pinners take the rows in the same sequence and queue rather than deadlock,
// the same discipline the row-at-a-time pinners follow.
func pinSessionBlobsTx(ctx context.Context, tx pgx.Tx, sessionID int64) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO blob_pins (sha256, expires_at)
		 SELECT sha, now() + $2 FROM (
		     SELECT input_sha256 AS sha FROM tool_calls WHERE session_id = $1 AND input_sha256 IS NOT NULL
		     UNION
		     SELECT result_sha256 FROM tool_calls WHERE session_id = $1 AND result_sha256 IS NOT NULL
		     UNION
		     SELECT sha256 FROM attachments WHERE session_id = $1
		 ) refs
		 ORDER BY sha
		 ON CONFLICT (sha256) DO UPDATE SET expires_at = EXCLUDED.expires_at`,
		sessionID, blobPinTTL)
	if err != nil {
		return fmt.Errorf("pin referenced blobs for session %d: %w", sessionID, err)
	}
	return nil
}

// BlobMeta returns a blob's stored size, media type, and storage content type
// without reading its body. The content type lets the serve path set Content-Encoding
// so the client decodes a compressed blob, while the server never touches the bytes.
func (s *Store) BlobMeta(ctx context.Context, sha256hex string) (Blob, error) {
	var b Blob
	b.SHA256 = sha256hex
	err := s.Pool.QueryRow(ctx,
		"SELECT byte_len, media_type, content_type FROM blobs WHERE sha256 = $1", sha256hex).
		Scan(&b.ByteLen, &b.MediaType, &b.ContentType)
	if errors.Is(err, pgx.ErrNoRows) {
		return Blob{}, ErrNotFound
	}
	return b, err
}

// WriteBlobTo streams a blob's whole body to w and returns its media type. Large
// object reads must run in a transaction, so the copy happens inside one.
func (s *Store) WriteBlobTo(ctx context.Context, w io.Writer, sha256hex string) (mediaType string, err error) {
	return s.WriteBlobPrefixTo(ctx, w, sha256hex, 0)
}

// WriteBlobPrefixTo streams at most limit bytes of a blob's body to w and returns its
// media type; limit <= 0 means the whole body. The large-object reader pulls only the
// bytes it copies, so a small limit transfers a small prefix rather than the whole
// object: a capped preview of a bulky CAS body is O(limit), not O(blob). A caller that
// needs to flag truncation compares limit against the stored byte_len from BlobMeta.
func (s *Store) WriteBlobPrefixTo(ctx context.Context, w io.Writer, sha256hex string, limit int64) (mediaType string, err error) {
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
		var src io.Reader = lo
		if limit > 0 {
			src = io.LimitReader(lo, limit)
		}
		_, err = io.Copy(w, src)
		return err
	})
	return mediaType, err
}

// SessionReferencesBlob reports whether a session points at a blob, through a
// tool call's input or result or through an attachment. Blob serving is gated on
// this so a session can never read a blob it does not reference, even though the
// CAS dedupes content across sessions.
//
// Lifting Codex images to the CAS means a session view fetches a blob per rendered
// image as well as per tool body, so this runs once per blob on every open and live
// refresh: it must stay logarithmic in the session's references, not scan them. Each
// arm is a hash-leading index lookup: tool_calls by (input_sha256, session_id) and
// (result_sha256, session_id), attachments by (sha256, session_id). The parameter is
// cast to char(64) so it matches the bpchar columns and their indexes; a bare text
// parameter would compare bpchar against text, cast the columns, and fall back to
// scanning the session's whole slice of tool calls and attachments.
func (s *Store) SessionReferencesBlob(ctx context.Context, sessionID int64, sha256hex string) (bool, error) {
	var ok bool
	err := s.Pool.QueryRow(ctx,
		`SELECT EXISTS (
		   SELECT 1 FROM tool_calls
		    WHERE session_id = $1 AND (input_sha256 = $2::char(64) OR result_sha256 = $2::char(64))
		   UNION ALL
		   SELECT 1 FROM attachments
		    WHERE session_id = $1 AND sha256 = $2::char(64)
		 )`, sessionID, sha256hex).Scan(&ok)
	return ok, err
}

// SweepBlobs deletes every blob no live row references, unlinking its large
// object. Liveness is computed, not refcounted, so the sweep is self-healing: it
// is only needed after a delete or re-parse, the only events that can orphan a
// blob. It returns the number of blobs removed.
//
// A freshly uploaded body the client has not yet referenced from a transcript is
// protected by a pin (see PutBlob): the orphan predicate excludes any blob that
// still has a blob_pins row after the reap, so the gap between uploading a body and
// uploading the transcript that references it cannot lose the body. Expired pins are
// reaped first so a body whose transcript never arrived is eventually reclaimable.
//
// The reap and the orphan predicate are read together, not each in isolation. The
// reap deletes expired pins through SKIP LOCKED, so the only expired pin rows that
// survive it are ones a refresher holds locked mid-upsert: those are about to become
// unexpired, so they must keep their blob, yet their committed expires_at still reads
// as expired. That is why the orphan predicate keys on the mere existence of a pin
// row rather than on expires_at > now(): had it trusted the committed expiry it would
// classify a blob whose pin is mid-refresh as unpinned and cascade the blob (and the
// pin the refresher is about to commit) away. A refresh touches only expires_at, not
// the FK column, so it takes no FOR KEY SHARE on the blobs row to stop the sweep on
// its own.
const sweepBlobBatchSize = 256

func (s *Store) SweepBlobs(ctx context.Context) (int, error) {
	if err := s.reapExpiredBlobPins(ctx); err != nil {
		return 0, err
	}

	removed := 0
	for {
		n, err := s.sweepBlobBatch(ctx)
		if err != nil {
			// n is added only after its transaction commits. A cancellation or
			// database error therefore returns the exact durable progress from the
			// earlier batches instead of reporting work that rolled back.
			return removed, err
		}
		removed += n
		if s.sweepBatchCommittedHook != nil {
			s.sweepBatchCommittedHook(n)
		}
		if n < sweepBlobBatchSize {
			return removed, nil
		}
	}
}

// reapExpiredBlobPins removes old pins in committed, deterministic batches.
// A locked row is being refreshed and is skipped; a short batch means every
// currently available expired pin has been handled.
func (s *Store) reapExpiredBlobPins(ctx context.Context) error {
	for {
		var reaped int64
		err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx,
				`DELETE FROM blob_pins p
				  USING (
				    SELECT sha256 FROM blob_pins
				     WHERE expires_at <= now()
				     ORDER BY sha256
				     LIMIT $1
				     FOR UPDATE SKIP LOCKED
				  ) expired
				  WHERE p.sha256 = expired.sha256`, sweepBlobBatchSize)
			if err != nil {
				return fmt.Errorf("clear expired blob pins: %w", err)
			}
			reaped = tag.RowsAffected()
			return nil
		})
		if err != nil {
			return err
		}
		if reaped < sweepBlobBatchSize {
			return nil
		}
	}
}

// sweepBlobBatch locks and removes one sha-ordered orphan page. The large
// objects and their rows commit together, so a later batch failure cannot undo
// progress already reported by SweepBlobs.
func (s *Store) sweepBlobBatch(ctx context.Context) (int, error) {
	removed := 0
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		// FOR UPDATE conflicts with the FOR KEY SHARE a live writer holds on a
		// blob it is about to reference. SKIP LOCKED passes over those rows. A pin
		// is live by existence here: an expired pin skipped by the reap is locked
		// by a refresher and must still protect its blob.
		rows, err := tx.Query(ctx,
			`SELECT sha256, lo_oid FROM blobs b
			  WHERE NOT EXISTS (
			          SELECT 1 FROM tool_calls t
			           WHERE t.input_sha256 = b.sha256 OR t.result_sha256 = b.sha256)
			    AND NOT EXISTS (
			          SELECT 1 FROM attachments a WHERE a.sha256 = b.sha256)
			    AND NOT EXISTS (
			          SELECT 1 FROM blob_pins p WHERE p.sha256 = b.sha256)
			  ORDER BY b.sha256
			  LIMIT $1
			  FOR UPDATE SKIP LOCKED`, sweepBlobBatchSize)
		if err != nil {
			return err
		}
		type orphan struct {
			sha string
			oid uint32
		}
		orphans := make([]orphan, 0, sweepBlobBatchSize)
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
			tag, err := tx.Exec(ctx, "DELETE FROM blobs WHERE sha256 = $1", o.sha)
			if err != nil {
				return err
			}
			removed += int(tag.RowsAffected())
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return removed, nil
}
