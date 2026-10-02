package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/sre"
	sreaction "github.com/pg-sage/sidecar/internal/sre/action"
	srebench "github.com/pg-sage/sidecar/sre-bench"
)

// M7 wired into the integrated M5+M6 runtime (coordinator, 2026-10-02):
// the database's M5 error budget feeds the downgrade signal; M5's
// approved cancels and their verified recoveries become ledger outcomes
// of their family; the PGIncidentBench shard reports (core, M6 reactive,
// M6 runways) each feed their own families' promotion evidence.

func executedCancel(id int64, family string, rec sreaction.RecoveryState,
	attribution string) sreaction.ActionOutcome {
	return sreaction.ActionOutcome{ProposalID: sre.NewUUID(), Family: family,
		Class: sreaction.ActionCancelBackend, State: sreaction.ProposalExecuted,
		Recovery: rec, Attribution: attribution, ActionLogID: id,
		UpdatedAt: time.Now()}
}

func TestActionLedgerOutcomeMapping(t *testing.T) {
	failed := executedCancel(15, "lock_blocking", sreaction.RecoveryNone, "")
	failed.State = sreaction.ProposalFailed
	refused := executedCancel(16, "lock_blocking", sreaction.RecoveryNone, "")
	refused.State = sreaction.ProposalRefused
	other := executedCancel(17, "lock_blocking", sreaction.RecoveryRecovered,
		sreaction.AttributionSage)
	other.Class = "terminate_backend"
	cases := map[string]struct {
		in     sreaction.ActionOutcome
		result string // "" means not recorded
	}{
		"recovered by pg_sage": {executedCancel(11, "lock_blocking",
			sreaction.RecoveryRecovered, sreaction.AttributionSage),
			earned.ResultVerifiedRecovery},
		"connection pressure": {executedCancel(12, "connection_pressure",
			sreaction.RecoveryRecovered, sreaction.AttributionSage),
			earned.ResultVerifiedRecovery},
		"not recovered": {executedCancel(13, "lock_blocking",
			sreaction.RecoveryNotRecovered, sreaction.AttributionUnknown),
			earned.ResultNotRecovered},
		"failed": {failed, earned.ResultNotRecovered},
		"recovered by someone else": {executedCancel(18, "lock_blocking",
			sreaction.RecoveryRecovered, sreaction.AttributionExternal), ""},
		"recovered, cause unknown": {executedCancel(19, "lock_blocking",
			sreaction.RecoveryRecovered, sreaction.AttributionUnknown), ""},
		"inconclusive": {executedCancel(20, "lock_blocking",
			sreaction.RecoveryInconclusive, ""), ""},
		"still observing": {executedCancel(21, "lock_blocking",
			sreaction.RecoveryObserving, ""), ""},
		"refused": {refused, ""},
		"no action log": {executedCancel(0, "lock_blocking",
			sreaction.RecoveryRecovered, sreaction.AttributionSage), ""},
		"unknown family": {executedCancel(22, "slo_burn",
			sreaction.RecoveryRecovered, sreaction.AttributionSage), ""},
		"other class": {other, ""},
	}
	for name, c := range cases {
		got, ok := actionLedgerOutcome("orders", c.in)
		if c.result == "" {
			if ok {
				t.Errorf("%s: recorded %+v", name, got)
			}
			continue
		}
		if !ok || got.Result != c.result || got.Class != earned.ClassBackendCancel ||
			got.Level != earned.L2 || got.Family != earned.Family(c.in.Family) ||
			got.ActionLogID != c.in.ActionLogID || got.Database != "orders" ||
			got.Source != earned.SourceExecutor {
			t.Errorf("%s: outcome = %+v (%v), want %s", name, got, ok, c.result)
		}
	}
}

type fakeActionOutcomes struct {
	outs  []sreaction.ActionOutcome
	err   error
	since time.Time
}

func (f *fakeActionOutcomes) Outcomes(_ context.Context, since time.Time,
	_ int) ([]sreaction.ActionOutcome, error) {
	f.since = since
	return f.outs, f.err
}

func TestActionOutcomeFeedRecordsEachRunOnce(t *testing.T) {
	pool := autonomyPool(t)
	svc, err := newAutonomyLedgers(true).ledgerFor(context.Background(), pool,
		config.DefaultConfig().SRE.Autonomy)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().UnixNano() % 1_000_000_000
	src := &fakeActionOutcomes{outs: []sreaction.ActionOutcome{
		executedCancel(base+1, "connection_pressure", sreaction.RecoveryRecovered,
			sreaction.AttributionSage),
		executedCancel(base+2, "connection_pressure", sreaction.RecoveryNotRecovered,
			sreaction.AttributionUnknown),
		executedCancel(base+3, "connection_pressure", sreaction.RecoveryObserving, ""),
	}}
	feed := actionOutcomeFeed{source: src, ledger: svc, database: "orders"}
	for run, want := range []int{2, 0} {
		n, err := feed.RunOnce(context.Background())
		if err != nil || n != want {
			t.Fatalf("run %d recorded %d (%v), want %d", run, n, err, want)
		}
	}
	if time.Since(src.since) < 29*24*time.Hour {
		t.Fatalf("the feed reads only since %v; the safety window is 30 days", src.since)
	}
	ev, err := svc.Evidence(context.Background(), earned.FamilyConnections,
		earned.ClassBackendCancel)
	if err != nil || ev.Live.VerifiedL2 != 1 {
		t.Fatalf("connection_pressure/backend_cancel live = %+v (%v)", ev.Live, err)
	}
	src.err = errors.New("proposal store unavailable")
	if _, err := feed.RunOnce(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "proposal store unavailable") {
		t.Fatalf("source error = %v", err)
	}
}

type burningSummary struct{}

func (burningSummary) BudgetSummary(context.Context) (earned.BudgetSummary, error) {
	return earned.BudgetSummary{Database: "orders", FastBurning: true}, nil
}

// The binding's error budget reaches the limiter: a carried-over freeze is
// downgraded while the database's SLO budget burns.
func TestInstallAutonomyBindsTheErrorBudget(t *testing.T) {
	pool := autonomyPool(t)
	ledgers := newAutonomyLedgers(true)
	ex := autonomousExecutor(pool)
	if err := ledgers.install(context.Background(), ex, autonomyBinding{database: "orders",
		control: pool, monitored: pool, settings: config.DefaultConfig().SRE.Autonomy,
		budget: earned.NewSummaryBudget(burningSummary{})}); err != nil {
		t.Fatal(err)
	}
	doc := policy.UnattendedProfile()
	doc.MaintenanceWindows = []string{"always"}
	ex.EnableStandingPolicyDocument(doc, nil)
	got := ex.EvaluateCustodianProposal(context.Background(), freezeCustodianProposal())
	if got.Decision != executor.PolicyDecisionObserveOnly ||
		got.BlockedReason != string(policy.ReasonAutonomyDowngraded) {
		t.Fatalf("freeze while the budget burns = %+v", got)
	}
}

func shardReport(t *testing.T, dir string, at time.Time, families ...string) {
	t.Helper()
	rate := 1.0
	var cells []srebench.CellRecord
	for _, f := range families {
		cells = append(cells, srebench.CellRecord{Arm: "causal-graph", Family: f, Runs: 12,
			SafePass: srebench.Metric{K: 12, N: 12}, Top1: srebench.Metric{K: 11, N: 12},
			MechanismPrecision: &rate})
	}
	if _, _, err := srebench.WriteReport(dir, srebench.Report{Schema: srebench.ReportSchema,
		GeneratedAt: at, Gated: []string{"causal-graph"}, Cells: cells}); err != nil {
		t.Fatal(err)
	}
}

// CI writes one report per shard under pgincidentbench/{core,reactive,
// runway}; pointing bench_results_path at the parent ingests all three,
// and each family reads the newest report that scored it.
func TestBenchShardReportsFeedTheirFamilies(t *testing.T) {
	pool := autonomyPool(t)
	svc, err := newAutonomyLedgers(true).ledgerFor(context.Background(), pool,
		config.DefaultConfig().SRE.Autonomy)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	shards := map[string]time.Time{"core": now.Add(-3 * time.Minute),
		"reactive": now.Add(-2 * time.Minute), "runway": now.Add(-time.Minute)}
	shardReport(t, filepath.Join(root, "core"), shards["core"], "lock_blocking",
		"connection_pressure", "wal_retention", "plan_regression")
	shardReport(t, filepath.Join(root, "reactive"), shards["reactive"], "checkpoint_storm",
		"temp_file_explosion", "replication_lag", "lwlock_contention")
	shardReport(t, filepath.Join(root, "runway"), shards["runway"], "wraparound_runway",
		"disk_wal_runway", "sequence_runway")
	n, err := ingestBenchPath(context.Background(), svc, root)
	if err != nil || n != 3 {
		t.Fatalf("ingest = %d (%v), want the three shard reports", n, err)
	}
	for family, shard := range map[earned.Family]string{earned.FamilyLockBlocking: "core",
		earned.FamilyCheckpoint: "reactive", earned.FamilyLWLock: "reactive",
		earned.FamilyWraparound: "runway", earned.FamilySequence: "runway"} {
		ev, err := svc.Evidence(context.Background(), family,
			earned.ApplicableClasses(family)[0])
		if err != nil || ev.Bench == nil || !ev.Bench.GeneratedAt.Equal(shards[shard]) {
			t.Errorf("%s evidence bench = %+v (%v), want the %s shard", family, ev.Bench,
				err, shard)
		}
	}
	if n, err := ingestBenchPath(context.Background(), svc, root); err != nil || n != 0 {
		t.Fatalf("second ingest = %d (%v)", n, err)
	}
}
