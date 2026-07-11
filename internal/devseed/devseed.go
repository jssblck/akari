// Package devseed fills a local akari server with realistic example data for
// development. It is a dev-only convenience, not part of the production server.
//
// The approach is deliberately not synthetic: rather than fabricate message and
// usage rows, it creates a handful of demo accounts, then runs the real akari
// client in-process for a short, bounded window so it discovers this machine's
// actual agent session logs and pushes them through the server's true ingest and
// parse pipeline. The freshly ingested sessions all land under one uploader, so a
// final pass randomly reassigns them across the demo accounts, leaving the store
// looking as though several people had each backed up their own work.
package devseed

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"os"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jssblck/akari/internal/client/discover"
	"github.com/jssblck/akari/internal/client/resolve"
	"github.com/jssblck/akari/internal/client/syncer"
	"github.com/jssblck/akari/internal/client/upload"
	"github.com/jssblck/akari/internal/config"
	"github.com/jssblck/akari/internal/server/auth"
	"github.com/jssblck/akari/internal/server/store"
)

// roster is the pool of demo accounts, drawn from women in computing history. The
// first account created becomes the admin and owns the ingest token; the rest
// receive sessions only through the random reassignment pass. Usernames are the
// login handles; full names are shown in log output for context.
var roster = []struct{ Username, FullName string }{
	{"grace", "Grace Hopper"},
	{"ada", "Ada Lovelace"},
	{"anna", "Anna Winlock"},
	{"katherine", "Katherine Johnson"},
	{"radia", "Radia Perlman"},
	{"barbara", "Barbara Liskov"},
	{"margaret", "Margaret Hamilton"},
	{"evelyn", "Evelyn Boyd Granville"},
}

// Options configures a seed run. The zero value is unusable; applyDefaults fills
// in sensible defaults for everything except ServerURL, which the caller must
// supply (it is the base URL of the running server the client uploads through).
type Options struct {
	// ServerURL is the base URL of the running akari server the client uploads
	// to, e.g. http://localhost:8080. Required.
	ServerURL string
	// NumUsers is how many demo accounts to create, clamped to the roster size.
	NumUsers int
	// Password is the shared plaintext login password for every demo account, so
	// a developer can sign in to any of them.
	Password string
	// TimeLimit bounds how long the in-process client keeps starting new uploads;
	// the upload in flight when it elapses still finishes. Zero means no limit.
	TimeLimit time.Duration
	// Concurrency caps how many session files upload in parallel.
	Concurrency int
	// Force re-seeds even when the store already holds sessions: it clears the
	// existing sessions first, then re-ingests and re-shuffles. Without it, a
	// populated store is left untouched so the hook is cheap to re-run on every
	// `eph up`. The clean slate is what keeps re-ingest from creating duplicate
	// rows once a prior run moved sessions off the ingest account.
	Force bool
	// Logf receives progress lines; defaults to log.Printf.
	Logf func(format string, args ...any)
}

func (o *Options) applyDefaults() {
	if o.NumUsers <= 0 {
		o.NumUsers = 4
	}
	if o.NumUsers > len(roster) {
		o.NumUsers = len(roster)
	}
	if o.Password == "" {
		o.Password = "akari-dev"
	}
	if o.TimeLimit == 0 {
		o.TimeLimit = 30 * time.Second
	}
	if o.Concurrency <= 0 {
		o.Concurrency = 8
	}
	if o.Logf == nil {
		o.Logf = log.Printf
	}
}

// demoUser is one ensured account.
type demoUser struct {
	ID       int64
	Username string
	FullName string
	Created  bool // false when the account already existed
}

// Run performs a full seed: ensure the demo accounts, and (unless the store
// already holds sessions) ingest this machine's local agent sessions and spread
// them across those accounts. It is idempotent: the accounts are upserted, and
// the expensive ingest is skipped when sessions already exist and Force is unset.
func Run(ctx context.Context, st *store.Store, opts Options) error {
	opts.applyDefaults()
	if opts.ServerURL == "" {
		return fmt.Errorf("ServerURL is required")
	}
	logf := opts.Logf
	pool := st.Pool

	users, err := ensureUsers(ctx, pool, opts.NumUsers, opts.Password)
	if err != nil {
		return fmt.Errorf("ensure demo users: %w", err)
	}
	logf("dev-seed: %d demo account(s) ready (password %q): %s", len(users), opts.Password, rosterSummary(users))

	existing, err := countSessions(ctx, pool)
	if err != nil {
		return fmt.Errorf("count sessions: %w", err)
	}
	if existing > 0 && !opts.Force {
		logf("dev-seed: %d session(s) already present; skipping ingest (pass --force to re-seed)", existing)
		return nil
	}
	// A re-seed starts from a clean slate. The client identifies a session by
	// (its token's user, agent, source_session_id), so once the previous run moved
	// sessions off the ingest account, re-ingesting under that account would create
	// duplicate rows and the reassignment pass would then collide on the
	// (user_id, agent, source_session_id) uniqueness constraint. Dropping the prior
	// sessions first keeps re-ingest idempotent. Rows cascade to messages, tool
	// calls, usage, and attachments; the demo accounts are left in place.
	if existing > 0 && opts.Force {
		deleted, err := deleteAllSessions(ctx, pool)
		if err != nil {
			return fmt.Errorf("clear existing sessions: %w", err)
		}
		logf("dev-seed: --force cleared %d existing session(s) before re-seeding", deleted)
	}

	// The client attributes every upload to the token's owner, so ingest under the
	// first account and let the reassignment pass redistribute afterward.
	secret, err := auth.NewToken()
	if err != nil {
		return fmt.Errorf("generate ingest token: %w", err)
	}
	if _, err := st.CreateAPIToken(ctx, users[0].ID, "dev-seed", "ingest", auth.HashToken(secret)); err != nil {
		return fmt.Errorf("create ingest token: %w", err)
	}

	logf("dev-seed: ingesting local agent sessions for up to %s via %s", opts.TimeLimit, opts.ServerURL)
	stats, err := ingest(ctx, opts, secret)
	if err != nil {
		return fmt.Errorf("ingest local sessions: %w", err)
	}
	logf("dev-seed: ingest finished: %d discovered, %d uploaded, %d up-to-date, %d skipped, %d incomplete, %d failed%s",
		stats.discovered, stats.uploaded, stats.upToDate, stats.skipped, stats.incomplete, stats.failed, stoppedNote(stats.stopped))

	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	dist, err := reassignSessions(ctx, pool, userIDs(users), rng)
	if err != nil {
		return fmt.Errorf("reassign sessions: %w", err)
	}
	logf("dev-seed: reassigned %d session(s) across %d account(s): %s", totalCount(dist), len(dist), distSummary(users, dist))
	return nil
}

// ensureUsers upserts the first n roster accounts, sharing one password. The
// first account is made admin. Existing accounts are reused as-is (their password
// is not reset), so the call is safe to repeat. n is clamped to [1, roster size]
// so a caller can never index past the roster.
func ensureUsers(ctx context.Context, pool *pgxpool.Pool, n int, password string) ([]demoUser, error) {
	if n < 1 {
		n = 1
	}
	if n > len(roster) {
		n = len(roster)
	}
	out := make([]demoUser, 0, n)
	for i := 0; i < n; i++ {
		hash, err := auth.HashPassword(password)
		if err != nil {
			return nil, fmt.Errorf("hash password: %w", err)
		}
		entry := roster[i]
		id, created, err := upsertUser(ctx, pool, entry.Username, hash, i == 0)
		if err != nil {
			return nil, fmt.Errorf("upsert %q: %w", entry.Username, err)
		}
		out = append(out, demoUser{ID: id, Username: entry.Username, FullName: entry.FullName, Created: created})
	}
	return out, nil
}

// upsertUser inserts a user, or returns the existing one's id when the username
// is already taken. created reports whether a new row was written.
func upsertUser(ctx context.Context, pool *pgxpool.Pool, username, passwordHash string, isAdmin bool) (id int64, created bool, err error) {
	err = pool.QueryRow(ctx,
		`INSERT INTO users (username, password_hash, is_admin)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (username) DO NOTHING
		 RETURNING id`, username, passwordHash, isAdmin).Scan(&id)
	if err == nil {
		return id, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, false, err
	}
	// The row already existed, so the insert returned nothing; read its id.
	err = pool.QueryRow(ctx, `SELECT id FROM users WHERE username = $1`, username).Scan(&id)
	return id, false, err
}

// ingestStats tallies the in-process client run.
type ingestStats struct {
	discovered, uploaded, upToDate, skipped, failed, incomplete int
	stopped                                                     bool // true if the time limit cut the run short
}

// ingest runs the real akari client in-process: it discovers this machine's agent
// session files from their standard roots, resolves each to a git project, and
// uploads new bytes to opts.ServerURL with the given token, bounded by
// opts.TimeLimit. It reuses the same discover/resolve/syncer/upload packages as
// `akari sync` so the data lands through the exact production path.
//
// Unlike `akari sync`, the time limit is a hard cap on the whole run, not just on
// starting new files: the deadline context is passed to every in-flight upload,
// so when it elapses uploads are cancelled rather than left to finish. A seed must
// stay within its window even when a few local sessions are very large, since it
// runs as an unattended post-start hook that would otherwise block `eph up`. A
// session cancelled mid-upload is left partially ingested, which is fine for
// local browsing and is completed by the next real `akari sync`.
func ingest(ctx context.Context, opts Options, token string) (ingestStats, error) {
	var stats ingestStats
	home, err := os.UserHomeDir()
	if err != nil {
		return stats, fmt.Errorf("locate home directory: %w", err)
	}
	machine := config.ResolveMachine(config.Client{}, os.Getenv, os.Hostname)

	files, err := discover.Discover(discover.Roots(config.Client{}, os.Getenv, home), discover.Excluder{})
	if err != nil {
		return stats, fmt.Errorf("discover sessions: %w", err)
	}
	stats.discovered = len(files)
	if len(files) == 0 {
		return stats, nil
	}

	resolver := resolve.New()
	client := upload.New(upload.NewHTTPClient(), opts.ServerURL, token)
	sy := syncer.New(resolver, client, machine, false)

	runCtx := ctx
	if opts.TimeLimit > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, opts.TimeLimit)
		defer cancel()
	}

	var (
		mu       sync.Mutex
		firstErr error // a sample real upload failure, for a contextual error below
	)
	sem := make(chan struct{}, opts.Concurrency)
	var wg sync.WaitGroup
	for _, f := range files {
		if runCtx.Err() != nil {
			stats.stopped = true
			break
		}
		select {
		case sem <- struct{}{}:
		case <-runCtx.Done():
			stats.stopped = true
		}
		if stats.stopped {
			break
		}
		wg.Add(1)
		go func(f discover.File) {
			defer wg.Done()
			defer func() { <-sem }()
			r := sy.SyncOne(runCtx, f)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case r.Skipped:
				stats.skipped++
			case errors.Is(r.Err, context.Canceled), errors.Is(r.Err, context.DeadlineExceeded):
				// The deadline cut this upload short; not a real failure.
				stats.incomplete++
			case r.Err != nil:
				stats.failed++
				if firstErr == nil {
					firstErr = r.Err
				}
			case r.Action == upload.ActionUploaded, r.Action == upload.ActionReset:
				stats.uploaded++
			case r.Action == upload.ActionUpToDate:
				stats.upToDate++
			}
		}(f)
	}
	wg.Wait()

	// A handful of bad session files among many good ones is tolerable: the seed
	// still produced data, so the failures are reported in the stats and the run
	// proceeds. But failures with nothing uploaded at all signal a systemic
	// problem the caller must hear about (a wrong server URL, a server not
	// accepting connections, an invalid token, or 5xx), so --strict can fail and
	// the best-effort path can log it rather than silently "succeed" with no data.
	if stats.uploaded == 0 && stats.failed > 0 {
		return stats, fmt.Errorf("all %d upload attempt(s) failed with nothing ingested; first error: %w", stats.failed, firstErr)
	}
	return stats, nil
}

// reassignSessions gives every session a randomly chosen owner from userIDs, inside one
// transaction, and returns the resulting per-user count. The owning account is the only
// user-scoped field on a session (messages, tool calls, and usage rows hang off the session
// id), so this alone redistributes the data. Each (agent, source_session_id) pair stays on
// exactly one row, so the (user_id, agent, source_session_id) uniqueness constraint cannot
// be violated.
//
// Assignment is per family, not per session: a subagent rides with the session that spawned
// it (its parent), so a parent and its children land on one owner. That keeps the link the
// ingest path sets meaningful (a subagent belongs to the same person as its orchestrator)
// and the demo realistic, rather than splitting one run's parent and subagents across
// accounts. A top-level session is its own family, so a store with no subagents reassigns
// exactly as a flat per-session shuffle would.
func reassignSessions(ctx context.Context, pool *pgxpool.Pool, userIDs []int64, rng *rand.Rand) (map[int64]int, error) {
	if len(userIDs) == 0 {
		return nil, fmt.Errorf("no users to reassign to")
	}
	fam, err := sessionFamilies(ctx, pool)
	if err != nil {
		return nil, err
	}
	// Pick an owner once per family root, in a stable order, so the draw is deterministic
	// for a fixed seed and every member of a family resolves to the same account.
	rootUser := make(map[int64]int64)
	for _, f := range fam {
		if _, ok := rootUser[f.root]; !ok {
			rootUser[f.root] = userIDs[rng.Intn(len(userIDs))]
		}
	}
	// The updates run in ascending session id (sessionFamilies returns rows in id
	// order). This transaction locks many session rows one by one, and only a
	// single shared order keeps it from forming a lock cycle with the server's
	// concurrent writers (the parse worker's rebuilds, announce's subagent
	// linking) while ingest is still running.
	dist := make(map[int64]int, len(userIDs))
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		for _, f := range fam {
			uid := rootUser[f.root]
			if _, err := tx.Exec(ctx,
				`UPDATE sessions SET user_id = $1, updated_at = now() WHERE id = $2`, uid, f.id); err != nil {
				return err
			}
			dist[uid]++
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return dist, nil
}

func countSessions(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	var n int
	err := pool.QueryRow(ctx, `SELECT count(*) FROM sessions`).Scan(&n)
	return n, err
}

// deleteAllSessions removes every session row, returning the count deleted. The
// foreign keys from messages, tool calls, usage events, and attachments cascade,
// so the parsed projection goes with them. Orphaned CAS blobs are reclaimed by
// the server's periodic sweep, so they need no handling here.
func deleteAllSessions(ctx context.Context, pool *pgxpool.Pool) (int64, error) {
	tag, err := pool.Exec(ctx, `DELETE FROM sessions`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// famMember is one session paired with its family root: parent_session_id when the session
// is a subagent, otherwise the session's own id. Reassignment groups on the root.
type famMember struct{ id, root int64 }

// sessionFamilies lists every session with its family root, ordered by id. The order is
// doubly load-bearing: it makes the owner draw deterministic, and reassignSessions locks
// rows in this order inside one transaction, so it must be a single global order to stay
// deadlock-free against the server's writers. The root is the top-level session a family
// shares (the linker points
// every subagent straight at the session that spawned it, so a child's parent is itself a
// root), which coalesce collapses to the session's own id for a top-level session.
func sessionFamilies(ctx context.Context, pool *pgxpool.Pool) ([]famMember, error) {
	rows, err := pool.Query(ctx, `SELECT id, coalesce(parent_session_id, id) FROM sessions ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var fam []famMember
	for rows.Next() {
		var m famMember
		if err := rows.Scan(&m.id, &m.root); err != nil {
			return nil, fmt.Errorf("scan session family row: %w", err)
		}
		fam = append(fam, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate session families: %w", err)
	}
	return fam, nil
}

func userIDs(users []demoUser) []int64 {
	ids := make([]int64, len(users))
	for i, u := range users {
		ids[i] = u.ID
	}
	return ids
}

func totalCount(dist map[int64]int) int {
	n := 0
	for _, c := range dist {
		n += c
	}
	return n
}

func stoppedNote(stopped bool) string {
	if stopped {
		return " (time limit reached)"
	}
	return ""
}

// rosterSummary lists the ensured accounts, marking which were newly created.
func rosterSummary(users []demoUser) string {
	parts := make([]string, len(users))
	for i, u := range users {
		mark := "exists"
		if u.Created {
			mark = "new"
		}
		parts[i] = fmt.Sprintf("%s (%s, %s)", u.Username, u.FullName, mark)
	}
	return join(parts)
}

// distSummary renders the per-account session counts, in roster order.
func distSummary(users []demoUser, dist map[int64]int) string {
	parts := make([]string, 0, len(users))
	for _, u := range users {
		parts = append(parts, fmt.Sprintf("%s=%d", u.Username, dist[u.ID]))
	}
	return join(parts)
}

func join(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}
