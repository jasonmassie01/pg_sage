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
	"github.com/pg-sage/sidecar/internal/ledger"
)

var (
	standaloneAutonomyMu sync.Mutex
	standaloneAutonomy   *autonomy.Supervisor
)

type executorProposalRouter struct{ executor *executor.Executor }

func (r executorProposalRouter) Route(
	ctx context.Context, proposal autonomy.Proposal,
) error {
	candidate := executor.CustodianProposal{
		Feature: proposal.Feature, SQL: proposal.SQL,
		TargetObjects: append([]string(nil), proposal.TargetObjects...),
		Deadline:      proposal.Deadline,
		Evidence:      proposal.Evidence,
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
	}, rollbackSQL, queryIDs)
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
	router := executorProposalRouter{executor: exec}
	schemaGuard, err := autonomy.NewPostgresSchemaGuard(pool, database, router, auditor)
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

func startStandaloneAutonomy(
	ctx context.Context, pool *pgxpool.Pool, cfg *config.Config,
	database string, exec *executor.Executor,
) error {
	supervisor, err := newDatabaseAutonomy(pool, cfg, database, exec)
	if err != nil {
		return err
	}
	supervisor.Start(ctx)
	standaloneAutonomyMu.Lock()
	standaloneAutonomy = supervisor
	standaloneAutonomyMu.Unlock()
	return nil
}

func shutdownStandaloneAutonomy(ctx context.Context) {
	standaloneAutonomyMu.Lock()
	supervisor := standaloneAutonomy
	standaloneAutonomy = nil
	standaloneAutonomyMu.Unlock()
	if supervisor == nil {
		return
	}
	if err := supervisor.Shutdown(ctx); err != nil {
		logWarn("shutdown", "autonomy workers: %v", err)
	}
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
