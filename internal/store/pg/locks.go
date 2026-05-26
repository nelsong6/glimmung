package pg

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nelsong6/glimmung/internal/server"
)

// LocksStore is the Postgres-backed implementation of the lock primitive
// that previously lived on cosmos.Store. Replaces the Cosmos id-uniqueness
// + ETag IfMatch dance with a single atomic INSERT ... ON CONFLICT DO
// UPDATE statement, which is the natural shape for mutual exclusion with
// expiration in Postgres.
//
// The `locks` table is provisioned in pg/migrations.go:
//
//	CREATE TABLE locks (
//	  scope text NOT NULL,
//	  key text NOT NULL,
//	  holder_id text,
//	  state text CHECK (state IN ('held', 'released')),
//	  expires_at timestamptz,
//	  acquired_at timestamptz DEFAULT now(),
//	  PRIMARY KEY (scope, key)
//	)
//
// Stage 2i deletes the matching methods + helpers from cosmos.Store; until
// then cosmos.Store retains the `locks *azcosmos.ContainerClient` field
// only to support the one-shot Migrate call below.
type LocksStore struct {
	pool *pgxpool.Pool
}

// LockState describes a held lock as seen by readers. Returned by
// ListHeldByScope and used by cosmos.Store.ListIssues / ListTouchpoints
// during the transition window to populate the per-row IssueLockHeld
// display flag.
type LockState struct {
	Scope     string
	Key       string
	HolderID  string
	ExpiresAt *time.Time
}

// NewLocksStore returns a LocksStore backed by pool. The pool's
// migrations (RunMigrations in pg/migrations.go) must have applied
// successfully before the first call.
func NewLocksStore(pool *pgxpool.Pool) *LocksStore {
	return &LocksStore{pool: pool}
}

// ClaimLock atomically claims a lock keyed by (scope, key) for holderID
// with a TTL of ttlSeconds. Mirrors the cosmos.Store.ClaimLock contract:
//   - returns nil if newly claimed or taken over from an expired/released
//     prior holder
//   - returns *server.AlreadyRunningError if currently held by another
//     holder with a non-expired TTL
//   - returns any other error from the pool unchanged
//
// The whole acquire is one statement. No read-modify-write race window;
// no retry loop required. metadata is accepted for signature parity with
// cosmos.Store.ClaimLock but is currently ignored — the Cosmos doc shape
// stored it for diagnostic display and nothing relied on it for control
// flow. If a future caller needs metadata on the Postgres row, add a
// `metadata jsonb` column to the locks table and persist it here.
func (s *LocksStore) ClaimLock(ctx context.Context, scope, key, holderID string, ttlSeconds int, _ map[string]any) error {
	if s == nil || s.pool == nil {
		return fmt.Errorf("locks store not configured")
	}
	expiresAt := time.Now().UTC().Add(time.Duration(ttlSeconds) * time.Second)

	const acquireSQL = `
		INSERT INTO locks (scope, key, holder_id, state, expires_at, acquired_at)
		VALUES ($1, $2, $3, 'held', $4, now())
		ON CONFLICT (scope, key) DO UPDATE
		  SET holder_id  = EXCLUDED.holder_id,
		      state      = 'held',
		      expires_at = EXCLUDED.expires_at,
		      acquired_at = now()
		  WHERE locks.state = 'released' OR locks.expires_at < now()
		RETURNING holder_id, expires_at
	`
	var resHolder string
	var resExpires time.Time
	err := s.pool.QueryRow(ctx, acquireSQL, scope, key, holderID, expiresAt).Scan(&resHolder, &resExpires)
	if err == nil {
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("locks: acquire: %w", err)
	}

	// ON CONFLICT WHERE didn't match — someone else holds it. Read the
	// current row to populate AlreadyRunningError correctly.
	const inspectSQL = `
		SELECT holder_id, expires_at
		FROM locks
		WHERE scope = $1 AND key = $2
	`
	var heldBy string
	var existingExpires time.Time
	if scanErr := s.pool.QueryRow(ctx, inspectSQL, scope, key).Scan(&heldBy, &existingExpires); scanErr != nil {
		// The CAS lost but the row vanished — racing with a release.
		// Surface as an AlreadyRunningError with zero values so the
		// caller backs off; the next attempt will succeed.
		if errors.Is(scanErr, pgx.ErrNoRows) {
			return &server.AlreadyRunningError{}
		}
		return fmt.Errorf("locks: inspect after CAS loss: %w", scanErr)
	}
	return &server.AlreadyRunningError{HeldBy: heldBy, ExpiresAt: existingExpires}
}

// ClaimIssueLock is the convenience wrapper for the per-issue dispatch
// lock. Matches the cosmos.Store contract exactly.
func (s *LocksStore) ClaimIssueLock(ctx context.Context, project string, issueNumber int, holderID string, ttlSeconds int) error {
	return s.ClaimLock(ctx, "issue", issueKey(project, issueNumber), holderID, ttlSeconds, nil)
}

// ReleaseLock releases the lock if it's currently held by holderID.
// Returns true if the release happened. Mirrors the cosmos contract:
// best-effort, no error returned even if the row isn't found.
func (s *LocksStore) ReleaseLock(ctx context.Context, scope, key, holderID string) bool {
	if s == nil || s.pool == nil {
		return false
	}
	const releaseSQL = `
		UPDATE locks
		SET state = 'released'
		WHERE scope = $1 AND key = $2 AND holder_id = $3 AND state = 'held'
	`
	tag, err := s.pool.Exec(ctx, releaseSQL, scope, key, holderID)
	if err != nil {
		return false
	}
	return tag.RowsAffected() == 1
}

// ReleaseIssueLock is the convenience wrapper for the per-issue
// dispatch lock. Best-effort.
func (s *LocksStore) ReleaseIssueLock(ctx context.Context, project string, issueNumber int, holderID string) {
	s.ReleaseLock(ctx, "issue", issueKey(project, issueNumber), holderID)
}

// AnyLockHeld reports whether at least one lock with the given scope is
// currently held (state='held' AND not expired). Used by the SPA's
// inflight-lock pulse via the state snapshot.
func (s *LocksStore) AnyLockHeld(ctx context.Context, scope string) (bool, error) {
	if s == nil || s.pool == nil {
		return false, nil
	}
	const sql = `
		SELECT EXISTS (
		  SELECT 1 FROM locks
		  WHERE scope = $1
		    AND state = 'held'
		    AND (expires_at IS NULL OR expires_at > now())
		)
	`
	var held bool
	if err := s.pool.QueryRow(ctx, sql, scope).Scan(&held); err != nil {
		return false, fmt.Errorf("locks: any held: %w", err)
	}
	return held, nil
}

// ListHeldByScope returns all currently-held (and unexpired) locks for a
// given scope, keyed by lock key. Used by cosmos.Store.ListIssues and
// cosmos.Store.ListTouchpoints during the migration window to populate
// per-row "lock held" display flags. After Stage 2e/2g moves those
// methods to pg, this is the only read path for held locks.
func (s *LocksStore) ListHeldByScope(ctx context.Context, scope string) (map[string]LockState, error) {
	if s == nil || s.pool == nil {
		return map[string]LockState{}, nil
	}
	const sql = `
		SELECT key, holder_id, expires_at
		FROM locks
		WHERE scope = $1
		  AND state = 'held'
		  AND (expires_at IS NULL OR expires_at > now())
	`
	rows, err := s.pool.Query(ctx, sql, scope)
	if err != nil {
		return nil, fmt.Errorf("locks: list held by scope: %w", err)
	}
	defer rows.Close()

	out := map[string]LockState{}
	for rows.Next() {
		var key string
		var holder string
		var expires *time.Time
		if err := rows.Scan(&key, &holder, &expires); err != nil {
			return nil, fmt.Errorf("locks: scan held row: %w", err)
		}
		out[key] = LockState{
			Scope:     scope,
			Key:       key,
			HolderID:  holder,
			ExpiresAt: expires,
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("locks: iterate held rows: %w", err)
	}
	return out, nil
}

// IssueLockHeld is the per-(project, issue_number) lookup used by
// cosmos.Store's per-row check in ListIssues. Kept as a narrow API so
// callers don't need to know about the issueKey() encoding.
func (s *LocksStore) IssueLockHeld(ctx context.Context, project string, issueNumber int) (bool, error) {
	if s == nil || s.pool == nil {
		return false, nil
	}
	const sql = `
		SELECT EXISTS (
		  SELECT 1 FROM locks
		  WHERE scope = 'issue'
		    AND key = $1
		    AND state = 'held'
		    AND (expires_at IS NULL OR expires_at > now())
		)
	`
	var held bool
	if err := s.pool.QueryRow(ctx, sql, issueKey(project, issueNumber)).Scan(&held); err != nil {
		return false, fmt.Errorf("locks: issue lock held: %w", err)
	}
	return held, nil
}

// PRLockHeld is the per-(repo, pr_number) lookup. Same shape as the
// issue variant but scoped to "pr".
func (s *LocksStore) PRLockHeld(ctx context.Context, repo string, prNumber int) (bool, error) {
	if s == nil || s.pool == nil {
		return false, nil
	}
	const sql = `
		SELECT EXISTS (
		  SELECT 1 FROM locks
		  WHERE scope = 'pr'
		    AND key = $1
		    AND state = 'held'
		    AND (expires_at IS NULL OR expires_at > now())
		)
	`
	var held bool
	if err := s.pool.QueryRow(ctx, sql, fmt.Sprintf("%s#%d", repo, prNumber)).Scan(&held); err != nil {
		return false, fmt.Errorf("locks: pr lock held: %w", err)
	}
	return held, nil
}

// issueKey encodes a (project, issueNumber) pair as the lock key, exactly
// matching the encoding the cosmos store used. Keeping the same encoding
// means migrated rows from cosmos line up with new claims from pg without
// any key rewriting.
func issueKey(project string, issueNumber int) string {
	return fmt.Sprintf("%s#%d", project, issueNumber)
}
