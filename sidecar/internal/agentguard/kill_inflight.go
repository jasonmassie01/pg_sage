package agentguard

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// In-flight agent requests (§6.10 step 4, §6.2.7). The executor (for an
// agent's apply) and the broker (for a brokered statement) register the
// backend running it in the control database's sage.guard_inflight; a
// kill cancels those backends, so an apply holding its principal FOR
// SHARE ends and the kill's FOR UPDATE goes through.

var uuidPattern = regexp.MustCompile(
	`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

type execer interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

// RegisterInflight records that backend pid on database databaseID runs a
// request of principalID (actionID 0: not an executor action).
func RegisterInflight(ctx context.Context, control execer, principalID, databaseID string,
	pid int, actionID int64) error {
	switch {
	case !ValidID(principalID):
		return invalid("principal id %q is not valid", principalID)
	case !uuidPattern.MatchString(databaseID):
		return invalid("database id %q is not a uuid", databaseID)
	case pid <= 0:
		return invalid("backend pid must be positive")
	case control == nil:
		return ErrUnavailable
	}
	_, err := control.Exec(ctx, `/* pg_sage guard_inflight v1 */
		INSERT INTO sage.guard_inflight (principal_id, database_id, backend_pid, action_id)
		VALUES ($1, $2::uuid, $3, NULLIF($4::bigint, 0))
		ON CONFLICT (database_id, backend_pid) DO UPDATE SET
			principal_id = EXCLUDED.principal_id, action_id = EXCLUDED.action_id,
			started_at = now()`, principalID, databaseID, pid, actionID)
	if err != nil {
		return fmt.Errorf("agentguard: registering an in-flight request: %w", err)
	}
	return nil
}

// UnregisterInflight removes a backend's in-flight record.
func UnregisterInflight(ctx context.Context, control execer, databaseID string,
	pid int) error {
	if !uuidPattern.MatchString(databaseID) {
		return invalid("database id %q is not a uuid", databaseID)
	}
	if control == nil {
		return ErrUnavailable
	}
	_, err := control.Exec(ctx, `/* pg_sage guard_inflight v1 */
		DELETE FROM sage.guard_inflight WHERE database_id = $1::uuid AND backend_pid = $2`,
		databaseID, pid)
	if err != nil {
		return fmt.Errorf("agentguard: removing an in-flight request: %w", err)
	}
	return nil
}

// ReconcileFallback records the local fallback log's entries in
// sage.action_log (each in its own database when that database is
// configured, else in the control database) and moves the log aside. It
// returns how many entries it recorded.
func (s *Switch) ReconcileFallback(ctx context.Context) (int, error) {
	if s.fallback == nil {
		return 0, nil
	}
	targets, err := s.targets(ctx)
	if err != nil {
		return 0, fmt.Errorf("agentguard: listing databases to reconcile into: %w", err)
	}
	pools := map[string]*pgxpool.Pool{}
	for _, t := range targets {
		pools[t.Name] = t.Pool
	}
	return s.fallback.drain(func(entries []FallbackEntry) error {
		for _, e := range entries {
			pool := pools[e.Database]
			if pool == nil {
				pool = s.store.Pool()
			}
			if pool == nil {
				return ErrUnavailable
			}
			if err := recordFallback(ctx, pool, e); err != nil {
				return err
			}
		}
		return nil
	})
}

func recordFallback(ctx context.Context, pool *pgxpool.Pool, e FallbackEntry) error {
	after, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("agentguard: encoding a fallback entry: %w", err)
	}
	var principal *string
	if e.PrincipalID != "" {
		principal = &e.PrincipalID
	}
	_, err = pool.Exec(ctx, `/* pg_sage guard_kill_record v1 */
		INSERT INTO sage.action_log (action_type, sql_executed, after_state, outcome,
			principal_id, executed_at, measured_at)
		VALUES ($1, $2, $3, $4, $5, $6, now())`, e.ActionType,
		trimStatements(e.Statements), after, e.Outcome, principal, e.At)
	if err != nil {
		return fmt.Errorf("agentguard: reconciling a kill fallback entry: %w", err)
	}
	return nil
}
