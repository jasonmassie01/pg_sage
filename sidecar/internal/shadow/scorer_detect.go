package shadow

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pg-sage/sidecar/internal/apppath"
	"github.com/pg-sage/sidecar/internal/catalogread"
)

// A change pg_sage wanted, applied outside pg_sage (a migration, an
// operator in psql), is found in the catalog by the detector of its class
// (earned-05):
//   - index_create, index_drop: an index of the same shape on the table,
//     or the index gone while its table stays (scorer_external.go);
//   - vacuum, analyze: a manual run after the decision was recorded
//     (pg_stat_user_tables.last_vacuum / last_analyze; autovacuum's runs
//     never count), judged by the decision's baseline against the metric
//     read in the pass that first sees the run (scorer_maintenance.go);
//   - autovacuum_tuning: the table's reloptions hold exactly the proposed
//     key=value pairs; statistics: an extended statistics object on the
//     table of the same columns and kinds (scorer_config.go).
//
// config_guc, query_hint and reindex have no detector and are never
// external evidence: settings change for unrelated reasons, and a check
// from pass to pass misses a change reverted in between. Every detector
// excludes the changes pg_sage made itself (earned-03): a sage.action_log
// row, not failed, on the same object since the decision. Detectors only
// read catalogs, statistics views and sage tables.

// catalogReader serves the catalog reads of the detectors (the pool).
type catalogReader interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// detector is a class's check: the verification of p's change applied
// outside pg_sage (nil when nothing changed, Unscorable when the catalog
// cannot tell), and own when the change is there but pg_sage made it (or
// may have).
type detector func(s *Scorer, ctx context.Context, p pendingDecision) (*verifiedFact, bool,
	error)

var detectors = map[string]detector{
	"index_create":      (*Scorer).indexExternal,
	"index_drop":        (*Scorer).indexExternal,
	"vacuum":            (*Scorer).maintenanceExternal,
	"analyze":           (*Scorer).maintenanceExternal,
	"autovacuum_tuning": (*Scorer).reloptionExternal,
	"statistics":        (*Scorer).statisticsExternal,
}

// external runs the detector of p's class; nothing for a class without.
func (s *Scorer) external(ctx context.Context, p pendingDecision) (*verifiedFact, bool,
	error) {
	detect, ok := detectors[p.Class]
	if !ok {
		return nil, false, nil
	}
	return detect(s, ctx, p)
}

// unscorableReason names a catalog read that can never succeed for this
// decision (a permission pg_sage lacks, a relation gone, a name that does
// not parse): the decision is
// left to the horizon with that reason, never an error of the pass.
func unscorableReason(err error) (string, bool) {
	var pg *pgconn.PgError
	if !errors.As(err, &pg) {
		return "", false
	}
	switch pg.Code {
	case "42501":
		return fmt.Sprintf("permission denied (SQLSTATE 42501): %s", pg.Message), true
	case "42P01", "3F000":
		return fmt.Sprintf("relation gone (SQLSTATE %s): %s", pg.Code, pg.Message), true
	case "42602", "42601", "0A000": // to_regclass of a malformed name, PG14 and 15
		return fmt.Sprintf("the decision's relation name does not resolve (SQLSTATE %s): %s",
			pg.Code, pg.Message), true
	}
	return "", false
}

// catalogFailure is the outcome of a failed catalog read: unscorable when
// it can never succeed, else an error naming what was read.
func catalogFailure(what string, err error) (*verifiedFact, bool, error) {
	if reason, ok := unscorableReason(err); ok {
		return &verifiedFact{Unscorable: reason}, false, nil
	}
	return nil, false, fmt.Errorf("shadow: %s: %w", what, err)
}

// appName is a relation the decision's SQL names as written, named as the
// application resolves it (apppath.Resolve): schema-qualified, so the
// catalog reads find it on pg_sage's session pinned to pg_catalog, pg_temp
// (security-01c-n). A name not on the application's path is returned as
// written, and the catalog read decides (relation gone).
func (s *Scorer) appName(ctx context.Context, name string) (string, error) {
	if s.pool == nil {
		return "", ErrUnavailable
	}
	resolved, err := apppath.Resolve(ctx, s.pool, catalogread.Default(), name)
	if err != nil {
		return "", fmt.Errorf("shadow: %w", err)
	}
	if resolved == "" {
		return name, nil
	}
	return resolved, nil
}

// relationGone is the unscorable fact of a table that no longer resolves.
func relationGone(table string) *verifiedFact {
	return &verifiedFact{Unscorable: "relation gone: " + table + " no longer resolves"}
}

// ownTableActionsSQL is pg_sage's first action, not failed, that names a
// table since a decision; it walks idx_action_log_time.
const ownTableActionsSQL = `/* pg_sage */ SELECT l.id FROM sage.action_log l
	WHERE l.executed_at >= $1 AND l.outcome <> 'failed'
	  AND l.sql_executed ILIKE '%' || $2::text || '%'
	ORDER BY l.executed_at LIMIT 1`

// ownTableAction reports whether pg_sage ran any action on table since d
// was recorded (true when it cannot tell: no table name). Any action
// naming the table rules it out, so a change is external only when no
// pg_sage action could have made it.
func (s *Scorer) ownTableAction(ctx context.Context, d Decision, table string) (bool, error) {
	name := lastIdentifier(table)
	if name == "" {
		return true, nil
	}
	var id int64
	err := s.pool.QueryRow(ctx, ownTableActionsSQL, d.RecordedAt,
		likeEscaper.Replace(name)).Scan(&id)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("shadow: read own actions on %s for decision %d: %w", table,
			d.ID, err)
	}
	return true, nil
}

// verifyExternal finishes a detected change of a configuration class like
// an index change: excluded when pg_sage made it, marked applied on the
// pass that first finds it, then verified over the windows around it
// (verifyApplied). A marked change is asked again on every pass: pg_sage's
// action may be logged just after the change shows in the catalog.
func (s *Scorer) verifyExternal(ctx context.Context, p pendingDecision, table string,
	marked bool) (*verifiedFact, bool, error) {
	own, err := s.ownTableAction(ctx, p.Decision, table)
	if err != nil || own {
		return nil, own, err
	}
	if !marked {
		if err := s.markApplied(ctx, p.ID); err != nil {
			return nil, false, err
		}
		return &verifiedFact{Reason: "the after window has just started"}, false, nil
	}
	f, err := s.verifyApplied(ctx, p)
	return f, false, err
}
