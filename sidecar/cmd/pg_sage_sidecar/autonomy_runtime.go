package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/autonomy"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/ha"
	"github.com/pg-sage/sidecar/internal/ledger"
)

// executorProposalRouter hands custodian proposals to the executor. Every
// proposal carries the HA state: a replica, or a primary in failover safe
// mode, is marked IsReplica so the gate refuses mutations. Without an HA
// probe the target is treated as a replica (fail closed).
type executorProposalRouter struct {
	executor  *executor.Executor
	isReplica func(context.Context) bool
}

func (r executorProposalRouter) replica(ctx context.Context) bool {
	if r.isReplica == nil {
		return true
	}
	return r.isReplica(ctx)
}

func (r executorProposalRouter) Route(
	ctx context.Context, proposal autonomy.Proposal,
) error {
	candidate := executor.CustodianProposal{
		Feature: proposal.Feature, SQL: proposal.SQL,
		TargetObjects: append([]string(nil), proposal.TargetObjects...),
		Deadline:      proposal.Deadline,
		Evidence:      proposal.Evidence,
		IsReplica:     r.replica(ctx),
	}
	if strings.TrimSpace(proposal.SQL) == "" {
		r.executor.EvaluateCustodianProposal(ctx, candidate)
		return nil
	}
	return r.executor.SubmitCustodianProposal(ctx, candidate)
}

func (r executorProposalRouter) RouteVerifiedIndex(
	ctx context.Context, proposal autonomy.Proposal,
	rollbackSQL string, queryIDs []int64,
) error {
	return r.executor.SubmitVerifiedIndexProposal(ctx, executor.CustodianProposal{
		Feature: proposal.Feature, SQL: proposal.SQL,
		TargetObjects: append([]string(nil), proposal.TargetObjects...),
		Deadline:      proposal.Deadline,
		IsReplica:     r.replica(ctx),
	}, rollbackSQL, queryIDs)
}

// authorizeRetention adapts retention intents to the executor's gate.
func (r executorProposalRouter) authorizeRetention(
	ctx context.Context, intent autonomy.RetentionIntent,
) error {
	return r.executor.AuthorizeRetention(ctx, executor.RetentionRequest{
		Target: intent.Schema + "." + intent.Table, Column: intent.Column,
		Cutoff: intent.Cutoff, Window: intent.Window, BatchLimit: intent.BatchLimit,
		Candidates: intent.Candidates, IsReplica: r.replica(ctx),
	})
}

// haReplicaProbe reports replica or failover safe mode for one database.
func haReplicaProbe(pool *pgxpool.Pool) func(context.Context) bool {
	monitor := ha.New(pool, logStructuredWrapper)
	return func(ctx context.Context) bool {
		return monitor.Check(ctx) || monitor.InSafeMode()
	}
}

type autonomyLogReporter struct{}

func (autonomyLogReporter) Report(level, message string, fields map[string]any) {
	if level == "error" {
		logError("autonomy", "%s fields=%v", message, fields)
		return
	}
	logInfo("autonomy", "%s fields=%v", message, fields)
}

func newDatabaseAutonomy(
	pool *pgxpool.Pool, cfg *config.Config, database string, exec *executor.Executor,
) (*autonomy.Supervisor, error) {
	if pool == nil || cfg == nil || exec == nil {
		return nil, fmt.Errorf("autonomy runtime dependencies are incomplete")
	}
	freezeWorker := autonomy.NewPostgresFreezeCustodian(
		pool, database, cfg.Custodian.Freeze.RedBufferPct,
	)
	walWorker := autonomy.NewPostgresWALCustodian(
		pool, database, autonomy.PostgresWALOptions{
			AbandonAfter: time.Duration(
				cfg.Custodian.WAL.AbandonAfterMinutes) * time.Minute,
			DiskPctCeiling: cfg.Custodian.WAL.RetainedWALDiskPctCeiling,
			// Drop remains disabled until policy supplies opt-in and an owner allowlist.
			AllowDrop: false,
		},
	)
	auditor := ledger.NewService(ledger.NewPostgresRepository(pool))
	router := executorProposalRouter{executor: exec, isReplica: haReplicaProbe(pool)}
	schemaGuard, err := autonomy.NewPostgresSchemaGuard(
		pool, database, router, auditor, router.authorizeRetention)
	if err != nil {
		return nil, err
	}
	supervisor, err := autonomy.NewSupervisor([]autonomy.DatabaseWorkersConfig{{
		Database: database, Interval: autonomyInterval(cfg),
		Freeze: freezeWorker, WAL: walWorker, Schema: schemaGuard,
		Router:  router,
		Auditor: auditor, Reporter: autonomyLogReporter{},
	}})
	if err != nil {
		return nil, err
	}
	exec.WithPostDDLHook(func(context.Context) error {
		return supervisor.RequestSchemaGuard(database)
	})
	return supervisor, nil
}

func autonomyInterval(cfg *config.Config) time.Duration {
	interval := cfg.Analyzer.Interval()
	if interval < time.Minute {
		return time.Minute
	}
	return interval
}

func startInstanceAutonomy(
	ctx context.Context, workers *sync.WaitGroup, pool *pgxpool.Pool,
	cfg *config.Config, database string, exec *executor.Executor,
) error {
	supervisor, err := newDatabaseAutonomy(pool, cfg, database, exec)
	if err != nil {
		return err
	}
	startInstanceWorker(workers, func() {
		supervisor.Start(ctx)
		<-ctx.Done()
		drainCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := supervisor.Shutdown(drainCtx); err != nil {
			logWarn("autonomy", "database %s drain: %v", database, err)
		}
	})
	return nil
}
