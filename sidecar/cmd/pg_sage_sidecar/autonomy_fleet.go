package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/api"
	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/rollout"
)

// autonomyAPIDeps wires the autonomy routes: the ledgers, game days and,
// with a fleet and a control database, the fleet canary.
func autonomyAPIDeps(mgr *fleet.DatabaseManager, control *pgxpool.Pool) *api.AutonomyDeps {
	ledgers := processAutonomy()
	deps := &api.AutonomyDeps{Ledgers: ledgers.registry, GameDays: ledgers.gameDays}
	if mgr == nil || control == nil {
		return deps
	}
	opts := rollout.CanaryOptions{CanaryInstances: 1, RegressionLimitPct: 10,
		Settle: time.Minute,
		Log:    func(format string, args ...any) { logWarn("rollout", format, args...) }}
	if cfg != nil {
		c := cfg.SRE.Autonomy.Canary
		opts.CanaryInstances, opts.RegressionLimitPct = c.CanaryInstances, c.RegressionLimitPct
		opts.Settle = time.Duration(c.SettleSeconds) * time.Second
	}
	svc, err := rollout.NewCanaryService(fleetCanaryResolver(mgr),
		rollout.NewPostgresRunStore(control), opts)
	if err != nil {
		logError("rollout", "fleet canary unavailable: %v", err)
		return deps
	}
	deps.Canary = svc
	return deps
}

func fleetCanaryResolver(mgr *fleet.DatabaseManager) rollout.TargetResolver {
	return func(database string) (rollout.CanaryTarget, error) {
		inst := mgr.GetInstance(database)
		if inst == nil || inst.Pool == nil {
			return nil, fmt.Errorf("database %q is not in the fleet", database)
		}
		return fleetCanaryTarget{inst: inst}, nil
	}
}

// fleetCanaryTarget is one fleet database: its action log, verification,
// recommendations and executor (the operator-approved path, gated).
type fleetCanaryTarget struct{ inst *fleet.DatabaseInstance }

func (t fleetCanaryTarget) Name() string { return t.inst.Name }

// SourceAction reads an executed action and its verification verdict.
func (t fleetCanaryTarget) SourceAction(ctx context.Context, actionLogID int64) (
	rollout.SourceAction, error) {
	var a rollout.SourceAction
	err := t.inst.Pool.QueryRow(ctx, `/* pg_sage */ SELECT l.sql_executed,
		COALESCE(l.rollback_sql, ''), l.action_type, l.outcome, COALESCE(v.verdict, '')
		FROM sage.action_log l LEFT JOIN sage.verification v ON v.id = l.verification_id
		WHERE l.id = $1`, actionLogID).Scan(&a.SQL, &a.RollbackSQL, &a.ActionType,
		&a.Outcome, &a.Verification)
	if err != nil {
		return rollout.SourceAction{}, fmt.Errorf("read action %d on %s: %w", actionLogID,
			t.inst.Name, err)
	}
	return a, nil
}

// MatchingFinding finds an open recommendation of the same statement
// (whitespace, case and a trailing semicolon aside).
func (t fleetCanaryTarget) MatchingFinding(ctx context.Context, sql string) (int, bool,
	error) {
	var id int
	err := t.inst.Pool.QueryRow(ctx, `/* pg_sage */ SELECT id FROM sage.findings
		WHERE status = 'open' AND acted_on_at IS NULL AND resolved_at IS NULL
		  AND lower(regexp_replace(rtrim(btrim(recommended_sql), ';'), '\s+', ' ', 'g'))
		      = $1
		ORDER BY id DESC LIMIT 1`, normalizeSQL(sql)).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("find matching recommendation on %s: %w", t.inst.Name, err)
	}
	return id, true, nil
}

func normalizeSQL(sql string) string {
	fields := strings.Fields(strings.TrimSuffix(strings.TrimSpace(sql), ";"))
	return strings.ToLower(strings.Join(fields, " "))
}

// Execute runs the statement as the operator's approval of the finding.
func (t fleetCanaryTarget) Execute(ctx context.Context, findingID int, sql,
	rollbackSQL string, approvedBy *int) (int64, error) {
	if t.inst.Executor == nil {
		return 0, fmt.Errorf("database %s has no executor", t.inst.Name)
	}
	return t.inst.Executor.ExecuteManual(ctx, findingID, sql, rollbackSQL, approvedBy)
}

// Rollback runs the action's stored rollback.
func (t fleetCanaryTarget) Rollback(ctx context.Context, actionLogID int64,
	reason string) error {
	if t.inst.Executor == nil {
		return fmt.Errorf("database %s has no executor", t.inst.Name)
	}
	return t.inst.Executor.RollbackAction(ctx, actionLogID, reason)
}

// ActionResult reads an action's outcome and verification verdict.
func (t fleetCanaryTarget) ActionResult(ctx context.Context, actionLogID int64) (string,
	string, error) {
	a, err := t.SourceAction(ctx, actionLogID)
	return a.Outcome, a.Verification, err
}

// autonomyMCPBackend serves the MCP autonomy tools from the registry.
type autonomyMCPBackend struct{ registry *earned.Registry }

func (b autonomyMCPBackend) entry(database string) (earned.RegistryEntry, string, error) {
	if database == "" {
		dbs := b.registry.Databases()
		if len(dbs) != 1 {
			return earned.RegistryEntry{}, "", fmt.Errorf("%w: database is required",
				earned.ErrInvalidRequest)
		}
		database = dbs[0]
	}
	e, ok := b.registry.Lookup(database)
	if !ok {
		return earned.RegistryEntry{}, "", fmt.Errorf("%w: unknown database %q",
			earned.ErrInvalidRequest, database)
	}
	return e, database, nil
}

// GetAutonomy returns the ledger view of a database.
func (b autonomyMCPBackend) GetAutonomy(ctx context.Context,
	req mcp.AutonomyRequest) (any, error) {
	e, name, err := b.entry(req.Database)
	if err != nil {
		return nil, err
	}
	v, err := e.Service.View(ctx)
	if err != nil {
		return nil, err
	}
	if e.Limiter != nil {
		e.Limiter.Annotate(ctx, &v)
	}
	return map[string]any{"database": name, "enforced": b.registry.Enforced(), "view": v},
		nil
}

// DowngradeAutonomy lowers a pair (or a family) for an MCP principal.
func (b autonomyMCPBackend) DowngradeAutonomy(ctx context.Context,
	req mcp.AutonomyRequest, actor string) (any, error) {
	e, _, err := b.entry(req.Database)
	if err != nil {
		return nil, err
	}
	level, err := earned.ParseLevel(req.Level)
	if err != nil {
		return nil, err
	}
	states, err := e.Service.Downgrade(ctx, earned.DowngradeRequest{
		Family: earned.Family(req.Family), Class: earned.ActionClass(req.ActionClass),
		To: level, Actor: actor, Reason: req.Reason})
	if err != nil {
		return nil, err
	}
	return map[string]any{"states": states}, nil
}
