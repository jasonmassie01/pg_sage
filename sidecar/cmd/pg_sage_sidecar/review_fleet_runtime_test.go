package main

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	reviewfleet "github.com/pg-sage/sidecar/internal/review/fleet"
)

// review-04b wiring: the fleet tiers run in fleet and meta mode only, the
// triage role takes review.triage_model, and the loop runs one pass a
// month, then advances the fleet.

func TestReviewFleetOn_FleetAndMetaModeOnly(t *testing.T) {
	for name, c := range map[string]struct {
		mode, meta string
		enabled    bool
		want       bool
	}{
		"standalone":      {"standalone", "", true, false},
		"fleet":           {"fleet", "", true, true},
		"meta":            {"standalone", "postgres://meta", true, true},
		"fleet, disabled": {"fleet", "", false, false},
	} {
		conf := config.DefaultConfig()
		conf.Mode, conf.MetaDB, conf.Review.Enabled = c.mode, c.meta, c.enabled
		if got := reviewFleetOn(conf); got != c.want {
			t.Errorf("%s: reviewFleetOn = %v, want %v", name, got, c.want)
		}
	}
	if reviewFleetOn(nil) {
		t.Error("no config runs the fleet tiers")
	}
}

func TestTriageRole_TakesTheTriageModel(t *testing.T) {
	prev := cfg
	t.Cleanup(func() { cfg = prev })
	cfg = config.DefaultConfig()
	parent := config.LLMConfig{Enabled: true, Endpoint: "http://llm", Model: "big-model",
		TimeoutSeconds: 30}
	if got := derivedLLMConfig(parent, llmRoleTriage, true); got.Model != "big-model" ||
		got.Endpoint != "http://llm" || !got.Enabled {
		t.Fatalf("no triage model: %+v, want the general config", got)
	}
	cfg.Review.TriageModel = "small-model"
	if got := derivedLLMConfig(parent, llmRoleTriage, true); got.Model != "small-model" ||
		got.Endpoint != "http://llm" || got.TimeoutSeconds != 30 {
		t.Fatalf("triage model: %+v, want small-model on the general endpoint", got)
	}
	if derivedLLMConfig(parent, llmRoleTriage, false).Enabled {
		t.Fatal("a disallowed triage client is enabled")
	}
	if llmRoleTriage.String() != "general" {
		t.Fatalf("triage role ledger label %q, want general", llmRoleTriage.String())
	}
}

func seedGrades(t *testing.T, ctx context.Context, pool *pgxpool.Pool, db string,
	scores ...float64) {
	t.Helper()
	for _, s := range scores {
		if _, err := pool.Exec(ctx, `INSERT INTO sage.review_run (database_name, kind,
			rubric_version, status, overall_grade, overall_score, coverage_pct, finished_at)
			VALUES ($1, 'review', 'pg_sage-rubric/1', 'done', 'C', $2, 80, now())`,
			db, s); err != nil {
			t.Fatalf("seed a grade of %s: %v", db, err)
		}
	}
}

func TestReviewFleetLoopDB_OnePassAMonthThenAdvance(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	control := clusterPool(t, ctx, "rfleet_control")
	members := map[string]*pgxpool.Pool{
		"steady":  clusterPool(t, ctx, "rfleet_steady"),
		"falling": clusterPool(t, ctx, "rfleet_falling"),
	}
	seedGrades(t, ctx, members["steady"], "steady", 80, 80)
	seedGrades(t, ctx, members["falling"], "falling", 90, 70)
	loop := &reviewFleetLoop{store: reviewfleet.NewStore(control, "fleet:test"),
		members: func() map[string]*pgxpool.Pool { return members },
		settings: func() reviewfleet.Settings {
			return reviewfleet.Settings{DeepMax: 30, Workers: 2}
		}}
	res, err := loop.tick(ctx)
	if err != nil || res.PassID == 0 || res.Members != 2 || len(res.Scheduled) != 1 ||
		res.Scheduled[0].Database != "falling" || res.Fleet.Skipped != "" {
		t.Fatalf("first tick %+v (%v), want a pass scheduling falling", res, err)
	}
	var kind string
	if err := members["falling"].QueryRow(ctx, `SELECT kind FROM sage.review_trigger`).
		Scan(&kind); err != nil || kind != "fleet_outlier" {
		t.Fatalf("falling's trigger %q (%v), want fleet_outlier", kind, err)
	}
	again, err := loop.tick(ctx)
	var passes int
	if qerr := control.QueryRow(ctx, `SELECT count(*) FROM sage.review_fleet_pass`).
		Scan(&passes); qerr != nil || err != nil || again.PassID != 0 || passes != 1 {
		t.Fatalf("second tick %+v (%v), %d passes (%v): want an advance, no new pass",
			again, err, passes, qerr)
	}
}
