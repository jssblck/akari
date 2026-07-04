package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jssblck/akari/internal/quality"
)

// SessionCard is everything the session OG card renders from, read as one consistent
// snapshot by SessionCard: the project identity the heading uses, the session's title and
// rolled-up figures, its span, its gated quality grade, and the activity strip pre-bucketed
// to a bounded histogram. Grouping every source read into one repeatable-read snapshot is
// deliberate: the card was stitched from the handler's session-detail read and a later,
// separate activity read, so an append or reparse between them could cache a card whose foot
// totals described one session version while its activity strip described another. One
// snapshot means the totals, the grade, and the strip all describe the same instant.
type SessionCard struct {
	// Project identity for the heading. Kind selects the label form (a local project shows
	// its name, a remote one its key), the same choice the session page's heading makes.
	ProjectKind string
	ProjectName string
	ProjectKey  string
	// Title is the session's first user message, squashed and capped like the page's title,
	// empty when the session has no user message (so the card skips the title line).
	Title string
	// MessageCount and TotalTokens are the session's own rollups (the four token classes
	// folded), the same figures the session header shows.
	MessageCount int
	TotalTokens  int64
	// StartedAt and EndedAt bound the session's span. The card derives both its DURATION
	// figure and its activity strip from these two (never last_active_at), so the card's
	// duration matches the page's Duration tile, which dashes when either is absent.
	StartedAt *time.Time
	EndedAt   *time.Time
	// Grade is the session's letter grade, non-nil only when the session carries a gated
	// (current-version, non-stale) signals grade, so an unscored or superseded session dashes
	// the QUALITY figure rather than showing a stale letter.
	Grade *string
	// Activity is the session's usage bucketed over its span into the requested number of
	// buckets, each the summed all-class token volume of the events in that slice. It is nil
	// when the session has no measured span (no start, no end, or a non-positive interval),
	// which the card draws as an empty strip. Its length is exactly the requested bucket count
	// when a span exists, so the histogram is bounded no matter how many usage events a long
	// session carries: the bucketing happens in SQL, not by materializing one point per event.
	Activity []int64
}

// SessionCard reads all of one session's OG-card inputs as a single repeatable-read,
// read-only snapshot, bucketing the activity strip into at most buckets buckets in SQL. found
// is false when the session id is unknown. A single session is rebuilt atomically during a
// reparse (never a half-built row), so this snapshot reads either the wholly-old or the
// wholly-new session, never a torn mix, which is why the session card needs no reparse-lock
// gate the aggregate cards take: the one snapshot is consistency enough for a single session.
func (s *Store) SessionCard(ctx context.Context, sessionID int64, buckets int) (SessionCard, bool, error) {
	var (
		card  SessionCard
		found bool
	)
	err := pgx.BeginTxFunc(ctx, s.Pool,
		pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly},
		func(tx pgx.Tx) error {
			var err error
			card, found, err = s.sessionCardFrom(ctx, tx, sessionID, buckets)
			return err
		})
	if err != nil {
		return SessionCard{}, false, fmt.Errorf("session card snapshot for session %d: %w", sessionID, err)
	}
	return card, found, nil
}

// sessionCardFrom assembles the card from one querier: the facts read (project, title,
// rollups, span, and the gated grade) and then, only when the facts carry a positive span,
// the bucketed activity read. Both run on the same querier, so under SessionCard they share
// one MVCC snapshot. $2 gates the grade to the running scoring version so a superseded or
// stale grade reads as unscored. The grade also requires grade IS NOT NULL, which under the
// score/grade coupling constraint (migration 0040) is exactly SessionSignals.Scored() (score
// and grade are both set or both NULL): the session page's Quality tile shows a grade only
// when Scored() holds, so the card applies the equivalent predicate and never advertises a
// grade the page reads as unscored.
func (s *Store) sessionCardFrom(ctx context.Context, q querier, sessionID int64, buckets int) (SessionCard, bool, error) {
	var card SessionCard
	err := q.QueryRow(ctx,
		`SELECT p.kind, p.display_name, p.remote_key,
		        coalesce(title.content, ''),
		        s.message_count,
		        s.total_input_tokens + s.total_output_tokens
		          + s.total_cache_read_tokens + s.total_cache_write_tokens,
		        s.started_at, s.ended_at,
		        CASE WHEN `+signalsCurrent(2)+`
		                  AND sig.grade IS NOT NULL
		             THEN sig.grade END
		   FROM sessions s
		   JOIN projects p ON p.id = s.project_id
		   LEFT JOIN session_signals sig ON sig.session_id = s.id
		   `+titleLateralSQL+`
		  WHERE s.id = $1`,
		sessionID, quality.Version).Scan(
		&card.ProjectKind, &card.ProjectName, &card.ProjectKey,
		&card.Title, &card.MessageCount, &card.TotalTokens,
		&card.StartedAt, &card.EndedAt, &card.Grade)
	if errors.Is(err, pgx.ErrNoRows) {
		return SessionCard{}, false, nil
	}
	if err != nil {
		return SessionCard{}, false, fmt.Errorf("read session card facts: %w", err)
	}
	card.Title = squashSpaces(card.Title)

	// A card with a measured span gets its activity strip; one without (an undated or
	// still-running session) is left with a nil strip the renderer draws as an empty grid.
	if buckets > 0 && card.StartedAt != nil && card.EndedAt != nil && card.EndedAt.After(*card.StartedAt) {
		activity, err := s.sessionActivityBuckets(ctx, q, sessionID, *card.StartedAt, *card.EndedAt, buckets)
		if err != nil {
			return SessionCard{}, false, err
		}
		card.Activity = activity
	}
	return card, true, nil
}

// sessionActivityBuckets sums the session's dated usage into a fixed histogram over [start,
// end], returning a slice of length buckets where each entry is that slice's all-class token
// volume. The bucketing is done in SQL with width_bucket, so the read stays bounded by the
// bucket count rather than the session's usage-event count: a day-long session with thousands
// of events returns the same small slice a short one does. width_bucket returns buckets+1 for
// an event exactly at end (its right edge is exclusive), which is folded into the last bucket
// so a closing event still lands on the strip rather than past it.
func (s *Store) sessionActivityBuckets(ctx context.Context, q querier, sessionID int64, start, end time.Time, buckets int) ([]int64, error) {
	rows, err := q.Query(ctx,
		`SELECT width_bucket(
		          extract(epoch FROM ue.occurred_at),
		          extract(epoch FROM $2::timestamptz),
		          extract(epoch FROM $3::timestamptz),
		          $4) AS bkt,
		        sum(ue.input_tokens + ue.output_tokens
		            + ue.cache_read_tokens + ue.cache_write_tokens) AS vol
		   FROM usage_events ue
		  WHERE ue.session_id = $1
		    AND ue.occurred_at IS NOT NULL
		    AND ue.occurred_at >= $2 AND ue.occurred_at <= $3
		  GROUP BY bkt`,
		sessionID, start, end, buckets)
	if err != nil {
		return nil, fmt.Errorf("session activity buckets for session %d: %w", sessionID, err)
	}
	defer rows.Close()
	out := make([]int64, buckets)
	for rows.Next() {
		var bkt int
		var vol int64
		if err := rows.Scan(&bkt, &vol); err != nil {
			return nil, fmt.Errorf("scan activity bucket: %w", err)
		}
		// width_bucket is 1-based, with buckets+1 for a value at the exclusive upper edge;
		// clamp both ends so an off-by-one boundary event still lands in a real cell.
		idx := bkt - 1
		if idx < 0 {
			idx = 0
		}
		if idx >= buckets {
			idx = buckets - 1
		}
		out[idx] += vol
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate session activity buckets for session %d: %w", sessionID, err)
	}
	return out, nil
}

// PutSessionOGImage stores the rendered preview card for a published session, stamped
// with the instant the card's data was read (generatedAt), not the write time. It
// upserts on the one-per-session key with the same guard PutOverviewOGImage uses: the
// DO UPDATE fires only when EXCLUDED.generated_at >= the stored generated_at, so a
// render that read older rollups but finishes last cannot clobber a newer card and
// make stale content look fresh for a whole TTL. Ties win harmlessly, since the render
// is deterministic for a given session snapshot.
//
// It reports whether this card became the cached one: true when the row was inserted
// or the guarded update fired, false when a newer card was already present. The caller
// uses that to avoid serving bytes it rendered but did not store (see
// ogimage.GenerateSession), so the served image never diverges from the cache.
func (s *Store) PutSessionOGImage(ctx context.Context, sessionID int64, png []byte, generatedAt time.Time) (bool, error) {
	tag, err := s.Pool.Exec(ctx,
		`INSERT INTO session_og_images (session_id, png, generated_at)
		      VALUES ($1, $2, $3)
		 ON CONFLICT (session_id)
		 DO UPDATE SET png = EXCLUDED.png, generated_at = EXCLUDED.generated_at
		       WHERE EXCLUDED.generated_at >= session_og_images.generated_at`,
		sessionID, png, generatedAt)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// SessionOGImage loads the cached preview card for a session, addressed by id, or
// ErrNotFound when none is cached yet. It is a plain by-id read with no visibility
// join: the public serve path reads through PublicSessionCard, which folds in the
// visibility gate atomically. This by-id form backs the render path's own
// reconciliation (GenerateSession reloads the canonical card after a skipped guarded
// write) and the tests, where the visibility gate is not the property under test.
func (s *Store) SessionOGImage(ctx context.Context, sessionID int64) (OGImage, error) {
	var img OGImage
	err := s.Pool.QueryRow(ctx,
		`SELECT png, generated_at FROM session_og_images WHERE session_id = $1`,
		sessionID).Scan(&img.PNG, &img.GeneratedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return OGImage{}, ErrNotFound
	}
	return img, err
}

// PublicSessionCard resolves a session's public id to its numeric id and reads that
// session's cached Open Graph card in one query, gated on visibility = 'public'.
// Folding the public check, the id lookup, and the card read into a single statement
// keeps the /s/<public_id>/og.png serve atomic: a split (resolve the session, then
// read the card) leaves a window where a concurrent unpublish between the two steps
// could serve a card for a session that just went private. found is false when the
// public id is unknown or the session is not public, so the caller 404s the link. When
// found is true the session is public; card.PNG is nil when no card is cached yet (the
// LEFT JOIN yields NULLs), which the caller renders on demand from the session's own
// rollups by id.
func (s *Store) PublicSessionCard(ctx context.Context, publicID string) (int64, OGImage, bool, error) {
	var sessionID int64
	var png []byte
	var generatedAt *time.Time
	err := s.Pool.QueryRow(ctx,
		`SELECT s.id, o.png, o.generated_at
		   FROM sessions s
		   LEFT JOIN session_og_images o ON o.session_id = s.id
		  WHERE s.public_id = $1 AND s.visibility = 'public'`,
		publicID).Scan(&sessionID, &png, &generatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, OGImage{}, false, nil
	}
	if err != nil {
		return 0, OGImage{}, false, fmt.Errorf("read public session card for %q: %w", publicID, err)
	}
	var card OGImage
	if png != nil && generatedAt != nil {
		card = OGImage{PNG: png, GeneratedAt: *generatedAt}
	}
	return sessionID, card, true, nil
}

// DeleteExpiredSessionOGImages removes cached session cards stamped before the cutoff,
// the housekeeping the cleanup loop runs beside DeleteExpiredOGImages. A card for a
// shared session re-renders on demand, so pruning a stale one only discards bytes
// nobody is serving. It returns how many rows it removed.
func (s *Store) DeleteExpiredSessionOGImages(ctx context.Context, olderThan time.Time) (int64, error) {
	tag, err := s.Pool.Exec(ctx,
		`DELETE FROM session_og_images WHERE generated_at < $1`, olderThan)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
