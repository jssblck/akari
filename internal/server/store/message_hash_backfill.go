package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// messageHashBackfillBatchSize bounds each backfill UPDATE so a large corpus
// never sits behind one long-running transaction; see
// BackfillMessageContentHashes for why this loop exists at all.
const messageHashBackfillBatchSize = 5000

// messageHashBackfillPause separates consecutive batches, so the backfill
// shares the connection pool with live traffic instead of running a tight
// loop that starves it while catching up on a large corpus.
const messageHashBackfillPause = 100 * time.Millisecond

// BackfillMessageContentHashes fills content_sha256 and thinking_text_sha256
// for every message row still at migration 0049's unbackfilled sentinel
// (never a valid digest). The migration only adds the columns, their default,
// and a trigger that stamps new and rewritten rows; it deliberately does not
// backfill the existing corpus itself, because a full-table UPDATE inside the
// migration's own transaction would hold the ADD COLUMN's access-exclusive
// lock on messages, the hottest table, for as long as the backfill took (see
// migration 0041 for the established pattern of deferring bulk population out
// of the migration transaction). This instead commits in bounded,
// primary-key-ordered batches with a pause between them, so a large corpus
// never blocks concurrent reads or writes and the pool stays available to
// live traffic throughout the pass.
//
// It is safe to run concurrently with normal operation: a session a rebuild
// rewrites arrives with its rows already stamped by the messages_content_hashes
// trigger (rebuild deletes and re-inserts every row), so the backfill only
// ever touches rows no rebuild has reached yet, and FOR UPDATE SKIP LOCKED
// steps around any row a concurrent rebuild is rewriting right now rather than
// waiting on it. It is idempotent across restarts: the sentinel itself is the
// worklist, so a process that stops mid-backfill just resumes where the
// sentinel says work remains next time it runs.
func (s *Store) BackfillMessageContentHashes(ctx context.Context) (int64, error) {
	var total int64
	for {
		n, err := s.backfillMessageContentHashBatch(ctx)
		if err != nil {
			return total, err
		}
		total += n
		if n < messageHashBackfillBatchSize {
			return total, nil
		}
		select {
		case <-ctx.Done():
			return total, ctx.Err()
		case <-time.After(messageHashBackfillPause):
		}
	}
}

// backfillMessageContentHashBatch stamps up to messageHashBackfillBatchSize
// unbackfilled rows, ordered by primary key so repeated calls make steady
// forward progress across the table rather than repeatedly contending for the
// same rows. FOR UPDATE SKIP LOCKED lets it pass over a row a concurrent
// rebuild is mid-rewrite on; that row is either already correctly stamped by
// the rebuild's own insert or will be picked up by a later batch.
func (s *Store) backfillMessageContentHashBatch(ctx context.Context) (int64, error) {
	var n int64
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE messages m
			   SET content_sha256 = encode(sha256(convert_to(m.content, 'UTF8')), 'hex'),
			       thinking_text_sha256 = encode(sha256(convert_to(m.thinking_text, 'UTF8')), 'hex')
			  FROM (
			    SELECT session_id, ordinal
			      FROM messages
			     WHERE content_sha256 = ''
			     ORDER BY session_id, ordinal
			     LIMIT $1
			     FOR UPDATE SKIP LOCKED
			  ) todo
			 WHERE m.session_id = todo.session_id AND m.ordinal = todo.ordinal`,
			messageHashBackfillBatchSize)
		if err != nil {
			return fmt.Errorf("backfill message content hashes: %w", err)
		}
		n = tag.RowsAffected()
		return nil
	})
	return n, err
}
