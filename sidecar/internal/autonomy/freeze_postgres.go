package autonomy

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/custodian/freeze"
	"github.com/pg-sage/sidecar/internal/policy"
)

type PostgresFreezeCustodian struct {
	pool      *pgxpool.Pool
	database  string
	threshold freeze.Thresholds
	mu        sync.Mutex
	lastAt    time.Time
	lastXacts int64
}

func NewPostgresFreezeCustodian(
	pool *pgxpool.Pool, database string, redBufferPct float64,
) *PostgresFreezeCustodian {
	red, amber := freezeThresholds(redBufferPct)
	return &PostgresFreezeCustodian{
		pool: pool, database: database,
		threshold: freeze.Thresholds{RedBufferPct: red, AmberBufferPct: amber},
	}
}

func (c *PostgresFreezeCustodian) Scan(ctx context.Context) ([]Proposal, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rate, err := c.transactionRate(ctx)
	if err != nil {
		return nil, err
	}
	blocker, err := c.oldestXminBlocker(ctx)
	if err != nil {
		return nil, err
	}
	repack, err := c.pgRepackAvailable(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := c.pool.Query(ctx, freezeHorizonSQL)
	if err != nil {
		return nil, fmt.Errorf("read freeze horizons: %w", err)
	}
	defer rows.Close()
	result := make([]Proposal, 0)
	for rows.Next() {
		proposal, scanErr := c.scanResponseRow(rows, rate, blocker, repack)
		if scanErr != nil {
			return nil, scanErr
		}
		if proposal.SQL != "" || proposal.Plan != "" {
			result = append(result, proposal)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate freeze horizons: %w", err)
	}
	return result, nil
}

func (c *PostgresFreezeCustodian) scanResponseRow(
	row interface{ Scan(...any) error }, rate float64,
	blocker *freeze.XminBlocker, repack bool,
) (Proposal, error) {
	var sample freeze.HorizonSample
	var deadRatio float64
	if err := row.Scan(&sample.Schema, &sample.Table, &sample.XIDAge, &sample.XIDMaxAge,
		&sample.MultiXactAge, &sample.MultiXactMaxAge, &deadRatio); err != nil {
		return Proposal{}, fmt.Errorf("scan freeze response: %w", err)
	}
	sample.Database = c.database
	sample.XIDsPerSecond, sample.MultiXactsPerSecond = rate, rate
	assessment, err := freeze.EvaluateHorizon(time.Now(), sample, c.threshold)
	if err != nil {
		return Proposal{}, fmt.Errorf("evaluate freeze response: %w", err)
	}
	input := freeze.ResponseInput{Schema: sample.Schema, Table: sample.Table,
		Urgency: assessment.Urgency, DeadTupleRatio: deadRatio,
		BloatRatio: deadRatio, PGRepackAvailable: repack}
	if assessment.Urgency == freeze.UrgencyRed {
		input.Blocker = blocker
	}
	response, err := freeze.PlanResponse(input)
	if err != nil {
		return Proposal{}, err
	}
	return freezeResponseProposal(c.database, sample, assessment.Proposal, response), nil
}

func freezeResponseProposal(
	database string, sample freeze.HorizonSample,
	deadline freeze.Proposal, response freeze.Response,
) Proposal {
	feature := "freeze"
	if response.Kind == freeze.ResponseCancelBlocker ||
		response.Kind == freeze.ResponseTerminateBlocker {
		feature = "freeze_blocker"
	} else if response.Kind == freeze.ResponseTuneAutovacuum {
		feature = "autovacuum_tuning"
	}
	evidence := response.Evidence
	if evidence == nil {
		evidence = map[string]any{}
	}
	if response.Plan != "" {
		evidence["plan"] = response.Plan
	}
	return Proposal{Database: database, Feature: feature, SQL: response.SQL,
		Plan: response.Plan, Evidence: evidence,
		TargetObjects: []string{sample.Schema + "." + sample.Table},
		Deadline:      freezePolicyDeadline(deadline)}
}

func (c *PostgresFreezeCustodian) oldestXminBlocker(
	ctx context.Context,
) (*freeze.XminBlocker, error) {
	var blocker freeze.XminBlocker
	err := c.pool.QueryRow(ctx, oldestXminBlockerSQL).Scan(
		&blocker.PID, &blocker.XminAge, &blocker.User, &blocker.State, &blocker.Query)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("diagnose xmin blocker: %w", err)
	}
	return &blocker, nil
}

func (c *PostgresFreezeCustodian) pgRepackAvailable(ctx context.Context) (bool, error) {
	var available bool
	err := c.pool.QueryRow(ctx, `SELECT EXISTS(
		SELECT 1 FROM pg_extension WHERE extname='pg_repack')`).Scan(&available)
	return available, err
}

func (c *PostgresFreezeCustodian) scanRow(
	row interface{ Scan(...any) error }, rate float64,
) (Proposal, error) {
	var sample freeze.HorizonSample
	if err := row.Scan(
		&sample.Schema, &sample.Table, &sample.XIDAge, &sample.XIDMaxAge,
		&sample.MultiXactAge, &sample.MultiXactMaxAge,
	); err != nil {
		return Proposal{}, fmt.Errorf("scan freeze horizon: %w", err)
	}
	sample.Database = c.database
	sample.XIDsPerSecond, sample.MultiXactsPerSecond = rate, rate
	assessment, err := freeze.EvaluateHorizon(time.Now(), sample, c.threshold)
	if err != nil {
		return Proposal{}, fmt.Errorf("evaluate freeze horizon: %w", err)
	}
	if assessment.Proposal.SQL == "" {
		return Proposal{}, nil
	}
	return Proposal{
		Database: c.database, Feature: "freeze", SQL: assessment.Proposal.SQL,
		TargetObjects: []string{sample.Schema + "." + sample.Table},
		Deadline:      freezePolicyDeadline(assessment.Proposal),
	}, nil
}

func freezePolicyDeadline(proposal freeze.Proposal) *policy.DeadlineContext {
	if proposal.Urgency != freeze.UrgencyRed {
		return nil
	}
	return &policy.DeadlineContext{
		Kind: policy.DeadlineXID, Urgency: policy.UrgencyCritical,
		HardAt: proposal.Deadline.HardAt,
	}
}

func (c *PostgresFreezeCustodian) transactionRate(ctx context.Context) (float64, error) {
	var total int64
	if err := c.pool.QueryRow(ctx, `/* pg_sage */ SELECT
		COALESCE(xact_commit+xact_rollback,0) FROM pg_stat_database
		WHERE datname=current_database()`).Scan(&total); err != nil {
		return 0, fmt.Errorf("read transaction rate: %w", err)
	}
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	rate := 1.0
	if !c.lastAt.IsZero() && total > c.lastXacts {
		rate = float64(total-c.lastXacts) / now.Sub(c.lastAt).Seconds()
	}
	c.lastAt, c.lastXacts = now, total
	return math.Max(rate, 0.001), nil
}

func freezeThresholds(red float64) (float64, float64) {
	if red <= 0 || red >= 100 {
		red = 25
	}
	amber := red * 2
	if amber > 100 {
		amber = 100
	}
	return red, amber
}

const freezeHorizonSQL = `/* pg_sage */
SELECT n.nspname, c.relname, age(c.relfrozenxid)::bigint,
       current_setting('autovacuum_freeze_max_age')::bigint,
       mxid_age(c.relminmxid)::bigint,
       current_setting('autovacuum_multixact_freeze_max_age')::bigint,
	       CASE WHEN COALESCE(s.n_live_tup,0)+COALESCE(s.n_dead_tup,0)=0 THEN 0
            ELSE s.n_dead_tup::float8/(s.n_live_tup+s.n_dead_tup) END
FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
LEFT JOIN pg_stat_user_tables s ON s.relid=c.oid
WHERE c.relkind IN ('r','m')
AND n.nspname NOT IN ('pg_catalog','information_schema','pg_toast','sage')`

const oldestXminBlockerSQL = `/* pg_sage */ SELECT pid, age(backend_xmin)::bigint,
COALESCE(usename,''), COALESCE(state,''), LEFT(COALESCE(query,''),500)
FROM pg_stat_activity WHERE backend_xmin IS NOT NULL AND pid<>pg_backend_pid()
ORDER BY age(backend_xmin) DESC LIMIT 1`
