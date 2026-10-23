package agentguard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The kill's control-database steps (§6.10 steps 1 and 2): kill records,
// freeze flags, principal status, tokens and in-flight backends.

const sqlLockNotAvailable = "55P03"

func isLockTimeout(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == sqlLockNotAvailable
}

// freezeMark is what step 1 writes: principals to freeze (all = every
// non-retired one) and an optional database or fleet flag.
type freezeMark struct {
	all        bool
	ids        []string
	flagScope  string // "", "database" or "fleet"
	flagTarget string
	reason     string
	actor      string
	killID     int64
}

// startKill records a kill before its steps run.
func (s *Switch) startKill(ctx context.Context, req KillRequest) (int64, error) {
	pool := s.store.Pool()
	if pool == nil {
		return 0, ErrUnavailable
	}
	var id int64
	err := pool.QueryRow(ctx, `/* pg_sage guard_kill v1 */
		INSERT INTO sage.guard_kills (scope, target, reason, requested_by)
		VALUES ($1, $2, $3, $4) RETURNING id`, string(req.Scope), req.ID, req.Reason,
		req.Actor).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("agentguard: recording the kill: %w", err)
	}
	return id, nil
}

// finishKill stores the kill's report.
func (s *Switch) finishKill(ctx context.Context, rep KillReport) error {
	pool := s.store.Pool()
	if pool == nil || rep.KillID == 0 {
		return nil
	}
	raw, err := json.Marshal(rep)
	if err != nil {
		return fmt.Errorf("agentguard: encoding the kill report: %w", err)
	}
	_, err = pool.Exec(ctx, `/* pg_sage guard_kill v1 */
		UPDATE sage.guard_kills SET finished_at = now(), report = $2 WHERE id = $1`,
		rep.KillID, raw)
	if err != nil {
		return fmt.Errorf("agentguard: storing the kill report: %w", err)
	}
	return nil
}

// markFrozen runs step 1 once: FOR UPDATE on the principals' rows under
// the lock timeout, so it waits for in-flight applies holding FOR SHARE
// (§6.2.7), then freezes them, sets the flag and supersedes pending
// unfreeze requests. It returns the frozen principal ids.
func (s *Switch) markFrozen(ctx context.Context, m freezeMark) ([]string, error) {
	pool := s.store.Pool()
	if pool == nil {
		return nil, ErrUnavailable
	}
	var ids []string
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var err error
		if ids, err = lockPrincipals(ctx, tx, m, s.cfg.LockTimeout.Milliseconds()); err != nil {
			return err
		}
		return writeFreeze(ctx, tx, m, ids)
	})
	if err != nil {
		return nil, fmt.Errorf("agentguard: freezing principals: %w", err)
	}
	return ids, nil
}

func lockPrincipals(ctx context.Context, tx pgx.Tx, m freezeMark, timeoutMS int64) (
	[]string, error) {
	if _, err := tx.Exec(ctx, fmt.Sprintf(
		"/* pg_sage guard_kill v1 */ SET LOCAL lock_timeout = '%dms'", timeoutMS)); err != nil {
		return nil, err
	}
	if !m.all && len(m.ids) == 0 {
		return []string{}, nil
	}
	rows, err := tx.Query(ctx, `/* pg_sage guard_kill v1 */
		SELECT id FROM sage.guard_principals
		WHERE status <> 'retired' AND ($1 OR id = ANY($2))
		ORDER BY id FOR UPDATE`, m.all, m.ids)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	return ids, nil
}

func writeFreeze(ctx context.Context, tx pgx.Tx, m freezeMark, ids []string) error {
	steps := []struct {
		sql  string
		args []any
	}{
		{`UPDATE sage.guard_principals SET status = 'frozen', frozen_reason = left($2, 2000),
			updated_at = clock_timestamp() WHERE id = ANY($1)`, []any{ids, m.reason}},
		{`INSERT INTO sage.guard_freezes (scope, target, kill_id, reason, set_by)
			SELECT 'principal', t, NULLIF($2::bigint, 0), left($3, 2000), $4
			FROM unnest($1::text[]) AS t
			ON CONFLICT (scope, target) WHERE cleared_at IS NULL DO UPDATE
			SET kill_id = COALESCE(sage.guard_freezes.kill_id, EXCLUDED.kill_id)`,
			[]any{ids, m.killID, m.reason, m.actor}},
		{`INSERT INTO sage.guard_freezes (scope, target, kill_id, reason, set_by)
			SELECT $1, $2, NULLIF($3::bigint, 0), left($4, 2000), $5 WHERE $1 <> ''
			ON CONFLICT (scope, target) WHERE cleared_at IS NULL DO UPDATE
			SET kill_id = COALESCE(sage.guard_freezes.kill_id, EXCLUDED.kill_id)`,
			[]any{m.flagScope, m.flagTarget, m.killID, m.reason, m.actor}},
		{`UPDATE sage.guard_unfreeze_requests SET status = 'superseded', decided_at = now()
			WHERE status = 'pending' AND ((scope = 'principal' AND target = ANY($1))
			   OR (scope = $2 AND target = $3))`, []any{ids, m.flagScope, m.flagTarget}},
	}
	for _, st := range steps {
		if _, err := tx.Exec(ctx, "/* pg_sage guard_kill v1 */ "+st.sql, st.args...); err != nil {
			return err
		}
	}
	return nil
}

// revokeTokens revokes every live MCP token of the principals.
func (s *Switch) revokeTokens(ctx context.Context, ids []string, actor string) (int64,
	error) {
	pool := s.store.Pool()
	if pool == nil {
		return 0, ErrUnavailable
	}
	if len(ids) == 0 {
		return 0, nil
	}
	tag, err := pool.Exec(ctx, `/* pg_sage guard_kill v1 */
		UPDATE sage.mcp_tokens SET revoked_at = now(), revoked_by = left($2, 200)
		WHERE principal_id = ANY($1) AND revoked_at IS NULL`, ids, "kill: "+actor)
	if err != nil {
		return 0, fmt.Errorf("agentguard: revoking agent tokens: %w", err)
	}
	return tag.RowsAffected(), nil
}

// inflightRow is a backend running an agent's request.
type inflightRow struct {
	databaseID string
	pid        int
}

// inflight lists the in-flight backends of the principals (all: every one).
func (s *Switch) inflight(ctx context.Context, ids []string, all bool) ([]inflightRow,
	error) {
	pool := s.store.Pool()
	if pool == nil {
		return nil, ErrUnavailable
	}
	rows, err := pool.Query(ctx, `/* pg_sage guard_kill v1 */
		SELECT database_id::text, backend_pid FROM sage.guard_inflight
		WHERE $1 OR principal_id = ANY($2)`, all, ids)
	if err != nil {
		return nil, fmt.Errorf("agentguard: reading in-flight backends: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (inflightRow, error) {
		var x inflightRow
		return x, r.Scan(&x.databaseID, &x.pid)
	})
	if err != nil {
		return nil, fmt.Errorf("agentguard: reading in-flight backends: %w", err)
	}
	return out, nil
}

// Frozen reports a fleet or database freeze flag (D1); the gate's
// decide.Freezes. The principal's own freeze is its status, which the
// decider reads itself. Without the control database it fails closed with
// ErrUnavailable.
func (s *Switch) Frozen(ctx context.Context, _, database string) (bool, string,
	error) {
	pool := s.store.Pool()
	if pool == nil {
		return false, "", ErrUnavailable
	}
	var scope, reason string
	err := pool.QueryRow(ctx, `/* pg_sage guard_frozen v1 */
		SELECT scope, reason FROM sage.guard_freezes
		WHERE cleared_at IS NULL AND ((scope = 'fleet' AND target = '')
		   OR (scope = 'database' AND target = $1))
		ORDER BY set_at LIMIT 1`, database).Scan(&scope, &reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, "", nil
	}
	if err != nil {
		return false, "", fmt.Errorf("agentguard: reading freeze flags: %w", err)
	}
	return true, scope + " frozen: " + reason, nil
}
