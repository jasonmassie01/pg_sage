package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/benchsig"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Roadmap 2.4 wiring: each database's investigator asks its own trust
// ledger, at the moment of a disagreement, whether the model may
// override the causal graph's root for the investigation's family. A
// database without a ledger keeps model roots advisory.

func TestRegistryRootAuthority_NoLedgerIsAdvisory(t *testing.T) {
	a := registryRootAuthority{registry: earned.NewRegistry(true), database: "orders"}
	got, err := a.ModelRootAuthority(context.Background(), "lock_blocking", "gpt-4o-mini")
	if err != nil || got.Granted || !strings.Contains(got.Reason, "no trust ledger") {
		t.Fatalf("authority = %+v (%v)", got, err)
	}
	var nilRegistry registryRootAuthority
	if got, err := nilRegistry.ModelRootAuthority(context.Background(),
		"lock_blocking", "gpt-4o-mini"); err != nil || got.Granted {
		t.Fatalf("nil registry = %+v (%v)", got, err)
	}
}

// liftJSON is an unstamped report with one held-out record. The
// generation time keeps nanoseconds: two reports of the same counts made
// in one second would otherwise be one report (deduplicated by hash), and
// the newest-measurement rule would read the older ingestion.
func liftJSON(at time.Time, family string, k, n int) []byte {
	return []byte(fmt.Sprintf(`{"schema": "pg_sage.pgincidentbench.v1",
		"schema_revision": 2, "generated_at": %q,
		"llm": {"mode": "live", "model": "gpt-4o-mini"},
		"gated_arms": ["causal-graph", "causal-graph+llm"], "cells": [],
		"model_lift": [{"arm": "causal-graph+llm", "baseline": "causal-graph",
			"family": %q, "split": "held_out", "llm_mode": "live", "runs": 40,
			"safe_pass": {"k": 38, "n": 40}, "baseline_safe_pass": {"k": 30, "n": 40},
			"top1": {"k": 20, "n": 22}, "baseline_top1": {"k": 18, "n": 22},
			"override_precision": {"k": %d, "n": %d},
			"override_safe_pass": {"k": 36, "n": 40}, "inconclusive_runs": 0,
			"inconclusive_resolved_right": 0, "inconclusive_resolved_wrong": 0,
			"forbidden_actions": 0}]}`, at.UTC().Format(time.RFC3339Nano), family, k, n))
}

// ingestSignedLift ingests liftJSON stamped with the running build as a
// signed release report: since benches-09 only a signed or local-run,
// stamped lift counts. The caller runs asRelease before building svc.
func ingestSignedLift(t *testing.T, svc *earned.Service, at time.Time, family string,
	k, n int) {
	t.Helper()
	b := runningBuild()
	raw := strings.Replace(string(liftJSON(at, family, k, n)), `"generated_at"`,
		fmt.Sprintf(`"pg_sage_version": %q, "pg_sage_commit": %q, "generated_at"`,
			b.Version, b.Commit), 1)
	if _, err := svc.IngestBench(context.Background(), []byte(raw), earned.BenchIngest{
		Origin: earned.OriginSignedRelease, Actor: "release_bench", RequireBuild: true,
		Signature: &earned.ReportSignature{Identity: "https://github.com/jasonmassie01/" +
			"pg_sage/.github/workflows/ci.yml@refs/tags/v1.8.5", Issuer: benchsig.GitHubIssuer,
			Commit: b.Commit, SignedAt: time.Now().UTC()}}); err != nil {
		t.Fatalf("ingest a signed lift report: %v", err)
	}
}

func ledgerWithLift(t *testing.T, pool *pgxpool.Pool, k, n int) *earned.Registry {
	t.Helper()
	asRelease(t)
	ledgers := newAutonomyLedgers(true)
	svc, err := ledgers.ledgerFor(context.Background(), pool,
		fmt.Sprintf("w3a_%d", time.Now().UnixNano()), config.DefaultConfig().SRE.Autonomy)
	if err != nil {
		t.Fatal(err)
	}
	ingestSignedLift(t, svc, time.Now().Add(-time.Hour), "lock_blocking", k, n)
	// The investigator runs the model the report measured (llm-01).
	svc.WithRunningModel(func() string { return "gpt-4o-mini" })
	reg := earned.NewRegistry(true)
	reg.Register("orders", earned.RegistryEntry{Service: svc})
	return reg
}

func TestRegistryRootAuthority_ReadsTheDatabaseLedger(t *testing.T) {
	pool := autonomyPool(t)
	for _, c := range []struct {
		k, n int
		want bool
	}{{16, 16, true}, {15, 15, false}} {
		a := registryRootAuthority{registry: ledgerWithLift(t, pool, c.k, c.n),
			database: "orders"}
		got, err := a.ModelRootAuthority(context.Background(), "lock_blocking", "gpt-4o-mini")
		if err != nil || got.Granted != c.want || got.Reason == "" {
			t.Errorf("%d/%d: authority = %+v (%v), want granted %v", c.k, c.n, got, err,
				c.want)
		}
	}
}

// twoEdgeRunner is an idle-in-transaction holder with an ALTER queued
// behind it and a reader behind the ALTER (two open hypotheses).
type twoEdgeRunner struct{}

func (twoEdgeRunner) Run(_ context.Context, id probes.ID, _ probes.Args) probes.Result {
	res := probes.Result{ProbeID: id, Version: "v1", Status: probes.StatusEmpty,
		ObservedAt: time.Now()}
	edge := func(waiter, blocker int64, mode, state string, age float64,
		waiting bool) probes.Row {
		return probes.Row{"waiter_pid": waiter, "lock_type": "relation",
			"requested_mode": mode, "relation": "public.orders", "blocker_pid": blocker,
			"blocker_kind": "backend", "blocker_state": state, "blocker_waiting": waiting,
			"blocker_xact_age_s": age, "blocker_backend_start": time.Now().UTC()}
	}
	if id == probes.LockGraph {
		res.Status = probes.StatusOK
		res.Rows = []probes.Row{
			edge(20, 4242, "AccessExclusiveLock", "idle in transaction", 90, false),
			edge(30, 20, "AccessShareLock", "active", 1, true)}
	}
	return res
}

var openLine = regexp.MustCompile(`(?m)^- ([a-z][a-z0-9_]*) \[`)

// reversingModel ranks the open hypotheses of the prompt in reverse: it
// always contests a conclusive root with two open hypotheses.
func reversingModel(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body := string(raw)
		if i := strings.Index(body, "Open hypotheses"); i >= 0 {
			body = body[i:]
		}
		if i := strings.Index(body, "Ruled out"); i >= 0 {
			body = body[:i]
		}
		var open []string
		for _, m := range openLine.FindAllStringSubmatch(strings.ReplaceAll(body, `\n`,
			"\n"), -1) {
			open = append(open, m[1])
		}
		for i, j := 0, len(open)-1; i < j; i, j = i+1, j-1 {
			open[i], open[j] = open[j], open[i]
		}
		review, _ := json.Marshal(map[string]any{"ranking": open, "claims": []any{}})
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{
			"message":       map[string]any{"role": "assistant", "content": string(review)},
			"finish_reason": "stop"}}, "usage": map[string]int{"total_tokens": 200}})
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestSREInvestigator_AdoptsTheModelRootOnlyWithLedgerAuthority(t *testing.T) {
	pool := autonomyPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	for _, c := range []struct {
		k, n                    int
		model, authority, cause string
	}{{16, 16, liftModel, sre.ContestAdopted, ""},
		{15, 15, liftModel, sre.ContestAdvisory, "below 0.80"},
		{16, 16, "m", sre.ContestAdvisory, `the investigator runs "m"`}} {
		svc, err := newSREInvestigator(sreInvestigatorDeps{control: pool, monitored: pool,
			runner: twoEdgeRunner{}, name: "w3a_db", settings: reviewSettings(),
			llm: liftModelClient(reversingModel(t), c.model), dailyTokens: 500000,
			notices: &sre.OnceLog{}, runtimeKey: fmt.Sprintf("w3a:%d", time.Now().UnixNano()),
			logFn: func(string, string, ...any) {},
			rootAuthority: registryRootAuthority{registry: ledgerWithLift(t, pool, c.k, c.n),
				database: "orders"}})
		if err != nil {
			t.Fatalf("investigator: %v", err)
		}
		inv, _, err := svc.Coordinator().Start(ctx, sre.Trigger{CaseID: "case:w3a",
			Kind: sre.TriggerLock, Subject: "incident w3a", IdempotencyKey: "w3a"})
		if err != nil || svc.Coordinator().Investigate(ctx, inv.ID) != nil {
			t.Fatalf("investigate: %v", err)
		}
		d, err := svc.Detail(ctx, inv.ID)
		if err != nil {
			t.Fatal(err)
		}
		mc := d.Investigation.Summary.ModelContest
		if mc == nil || mc.Authority != c.authority || !strings.Contains(mc.Reason, c.cause) {
			t.Fatalf("%d/%d: contest = %+v (summary %+v)", c.k, c.n, mc,
				d.Investigation.Summary)
		}
		wantRoot := mc.GraphRoot
		if c.authority == sre.ContestAdopted {
			wantRoot = mc.ModelRoot
		}
		if d.Investigation.Summary.Root != wantRoot {
			t.Fatalf("%d/%d: root %q, want %q", c.k, c.n, d.Investigation.Summary.Root,
				wantRoot)
		}
	}
}
