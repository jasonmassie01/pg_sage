package main

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// sre-07 wiring: the plan-only EXPLAIN runs on the database's own probe
// runner (its slots) with the collector's in-memory statement texts.

func TestSREInvestigatorConfig_ExplainsOnTheDatabasesRunner(t *testing.T) {
	pool, err := pgxpool.New(context.Background(), "postgres://sage@127.0.0.1:1/db")
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	settings := config.DefaultConfig().SRE
	texts := func(int64) (string, bool) { return "SELECT 1", true }
	got := sreInvestigatorConfig(settings, probes.NewRunner(pool, probes.Catalog(), nil), texts)
	if got == nil || got.Explainer == nil {
		t.Fatalf("investigator config with a runner = %+v, want an explainer", got)
	}
	if got := sreInvestigatorConfig(settings, probes.NewRunner(nil, probes.Catalog(), nil),
		texts); got == nil || got.Explainer != nil {
		t.Fatalf("runner without a pool = %+v, want the investigator without EXPLAIN", got)
	}
	settings.LLM.Mode = config.SRELLMModeReview
	if got := sreInvestigatorConfig(settings, probes.NewRunner(pool, probes.Catalog(), nil),
		texts); got != nil {
		t.Fatalf("review mode = %+v, want no investigator", got)
	}
}

func TestStatementTexts_NilWithoutACollector(t *testing.T) {
	rt := &databaseRuntime{}
	if rt.statementTexts() != nil {
		t.Fatal("a runtime without a collector served statement texts")
	}
}
