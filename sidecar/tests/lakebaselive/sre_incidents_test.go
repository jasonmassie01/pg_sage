package lakebaselive

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/sre/probes"
	"github.com/pg-sage/sidecar/internal/sre/runbook"
)

type incidentAcceptance struct {
	Fixture    string   `json:"fixture"`
	Case       sre.UUID `json:"case"`
	Detected   bool     `json:"detected"`
	Diagnosed  bool     `json:"diagnosed"`
	Root       string   `json:"root"`
	Actionable bool     `json:"actionable"`
	Reason     string   `json:"action_reason"`
	Runbook    string   `json:"runbook_outcome,omitempty"`
}

func nativeDeterministicCase(t *testing.T, p *pgxpool.Pool, ctx context.Context, policy *probes.ReadPolicy,
	kind sre.TriggerKind, subject, wantRoot string, setup ...func(*sre.PostgresStore, sre.Scope)) sre.Investigation {
	t.Helper()
	store, err := sre.NewPostgresStore(p, sre.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	cfg := sre.DefaultCoordinatorConfig("native-incident:" + string(sre.NewUUID()))
	cfg.AutomaticStart = false
	coord, err := sre.NewCoordinator(sre.CoordinatorDeps{Store: store, Config: cfg,
		Runner: probes.NewRunner(p, probes.Catalog(), nil, policy), ReadPolicy: policy})
	if err != nil {
		t.Fatal(err)
	}
	scope, err := coord.Bind(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, prepare := range setup {
		prepare(store, scope)
	}
	trigger := sre.Trigger{CaseID: "acceptance:" + string(sre.NewUUID()), Kind: kind,
		Subject: subject, IdempotencyKey: "acceptance:" + string(sre.NewUUID()), Actor: "native-acceptance"}
	inv, created, err := coord.Start(ctx, trigger)
	if err != nil || !created {
		t.Fatalf("start: created=%v err=%v", created, err)
	}
	if err := coord.Investigate(ctx, inv.ID); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(ctx, scope, inv.ID)
	if err != nil || got.State != sre.StateConcluded || got.Summary.Root != wantRoot {
		t.Fatalf("known incident case=%s state=%s root=%s want=%s err=%v", inv.ID, got.State, got.Summary.Root, wantRoot, err)
	}
	if err := store.VerifyEvents(ctx, scope, inv.ID); err != nil {
		t.Fatal(err)
	}
	ev, err := store.Evidence(ctx, scope, inv.ID)
	if err != nil || len(ev) == 0 {
		t.Fatalf("missing durable incident evidence: %v", err)
	}
	for _, e := range ev {
		if !e.VerifyHash() {
			t.Fatal("incident evidence hash mismatch")
		}
	}
	t.Logf("preserved case=%s root=%s state=%s evidence=%d", got.ID, got.Summary.Root, got.State, len(ev))
	return got
}

func TestLakebaseSREControlledIncidents(t *testing.T) {
	p, ctx, policy := sreNativePool(t)
	name := fmt.Sprintf("sage_sre_acceptance_%d", time.Now().UnixNano())
	quoted := pgx.Identifier{name}.Sanitize()
	exec(t, p, ctx, "CREATE SCHEMA "+quoted)
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if _, err := p.Exec(cleanup, "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Errorf("owned fixture cleanup: %v", err)
			return
		}
		t.Logf("cleanup confirmed for owned schema %s", name)
	}()
	var before int
	if err := p.QueryRow(ctx, "SELECT count(*) FROM sage.action_log").Scan(&before); err != nil {
		t.Fatal(err)
	}
	var results []incidentAcceptance
	t.Run("OwnedRowLockAndIdleTransaction", func(t *testing.T) {
		table := pgx.Identifier{name, "locked_row"}.Sanitize()
		exec(t, p, ctx, "CREATE TABLE "+table+"(id int PRIMARY KEY, value int NOT NULL)")
		exec(t, p, ctx, "INSERT INTO "+table+" VALUES (1,0)")
		// This workload is deliberately not tagged as pg_sage monitoring: the
		// production probes must see the known application blocker and waiter.
		cfg := p.Config().Copy()
		cfg.ConnConfig.RuntimeParams["application_name"] = "sage_acceptance_owned_lock"
		cfg.MaxConns, cfg.MinConns = 2, 0
		fixture, err := pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer fixture.Close()
		holder, err := fixture.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer holder.Release()
		waiter, err := fixture.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer waiter.Release()
		var holderPID, waiterPID int
		if err := holder.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&holderPID); err != nil {
			t.Fatal(err)
		}
		if err := waiter.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&waiterPID); err != nil {
			t.Fatal(err)
		}
		htx, err := holder.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = htx.Rollback(context.Background()) }()
		if _, err := htx.Exec(ctx, "UPDATE "+table+" SET value=value+1 WHERE id=1"); err != nil {
			t.Fatal(err)
		}
		wtx, err := waiter.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := wtx.Exec(ctx, "SET LOCAL lock_timeout=0; SET LOCAL statement_timeout='90s'"); err != nil {
			t.Fatal(err)
		}
		waitCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
		done := make(chan error, 1)
		go func() { _, err := wtx.Exec(waitCtx, "UPDATE "+table+" SET value=value+1 WHERE id=1"); done <- err }()
		defer func() {
			cancel()
			cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
			defer stop()
			_ = htx.Rollback(cleanup)
			select {
			case <-done:
			case <-cleanup.Done():
				t.Error("owned waiter did not exit before cleanup deadline")
			}
			_ = wtx.Rollback(cleanup)
		}()
		runner := probes.NewRunner(p, probes.Catalog(), nil, policy)
		var found bool
		var last probes.Result
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			r := runner.Run(ctx, probes.LockGraph, probes.Args{})
			last = r
			edges, err := probes.LockEdges(r)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range edges {
				if e.BlockerPID == holderPID && e.WaiterPID == waiterPID && e.BlockerState == "idle in transaction" {
					found = true
				}
			}
			if found {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if !found {
			t.Fatalf("owned blocker/waiter identities not detected by database-scoped lock_graph: holder=%d waiter=%d result=%s", holderPID, waiterPID, last.Text(10))
		}
		var signed sre.Runbook
		inv := nativeDeterministicCase(t, p, ctx, policy, sre.TriggerLock, "owned native row-lock fixture", "idle_in_tx_holder",
			func(store *sre.PostgresStore, scope sre.Scope) {
				definition := runbook.Definition{Name: "Owned native idle holder",
					Trigger: runbook.Trigger{Kinds: []string{string(sre.TriggerLock)}, Nodes: []string{"idle_in_tx_holder"}},
					Start:   "read_graph", Nodes: []runbook.Node{
						{ID: "read_graph", Type: runbook.NodeProbe, Probe: "lock_graph", Next: "is_idle"},
						{ID: "is_idle", Type: runbook.NodeDecision, When: &runbook.Predicate{Op: runbook.OpHypothesis,
							Node: "idle_in_tx_holder", In: []string{"root_cause"}}, Then: "operator_step", Else: "escalate"},
						{ID: "operator_step", Type: runbook.NodeProposal, Proposal: &runbook.Proposal{Kind: runbook.ProposalOperatorStep, Node: "idle_in_tx_holder"}},
						{ID: "escalate", Type: runbook.NodeProposal, Proposal: &runbook.Proposal{Kind: runbook.ProposalEscalate}},
					}}
				var err error
				signed, err = store.CreateRunbook(ctx, scope, sre.RunbookInput{Definition: definition, Actor: "native-acceptance"})
				if err != nil {
					t.Fatal(err)
				}
				hash, err := definition.Hash()
				if err != nil {
					t.Fatal(err)
				}
				signed, err = store.SignRunbook(ctx, scope, signed.ID, sre.RunbookSignature{Version: 1,
					ContentHash: hash, Signer: "native-acceptance", Role: "admin"})
				if err != nil {
					t.Fatal(err)
				}
			})
		if inv.Summary.Subject != fmt.Sprintf("pid %d", holderPID) {
			t.Fatalf("diagnosis picked a different blocker: %s", inv.Summary.Subject)
		}
		run := inv.Summary.Runbook
		if run == nil || run.RunbookID != signed.ID || run.Version != 1 || run.ContentHash != signed.Latest.ContentHash ||
			run.Outcome != sre.RunbookCompleted || run.Probes != 1 || run.Proposal == nil || run.Proposal.Kind != "operator_step" ||
			strings.Join(run.Path, ",") != "read_graph,is_idle,operator_step" {
			t.Fatalf("native signed runbook did not preserve version/evidence/path: %+v", run)
		}
		results = append(results, incidentAcceptance{Fixture: "row_lock_idle_transaction", Case: inv.ID, Detected: true,
			Diagnosed: true, Root: inv.Summary.Root, Reason: probes.ReasonPolicyDenied, Runbook: string(run.Outcome)})
	})
	t.Run("AdvancingSequenceWithExplicitLimit", func(t *testing.T) {
		sequence := pgx.Identifier{name, "bounded_sequence"}.Sanitize()
		exec(t, p, ctx, "CREATE SEQUENCE "+sequence+" AS bigint MINVALUE 1 MAXVALUE 1000 START 900 NO CYCLE CACHE 1")
		exec(t, p, ctx, "SELECT nextval($1::regclass)", sequence)
		// Advance throughout the production two-sample plan. Neither its
		// interval nor the causal nearness/consumption thresholds are changed.
		advanceCtx, cancel := context.WithCancel(ctx)
		var wg sync.WaitGroup
		var advanceErr error
		wg.Add(1)
		go func() {
			defer wg.Done()
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-advanceCtx.Done():
					return
				case <-ticker.C:
					if _, err := p.Exec(advanceCtx, "SELECT nextval($1::regclass)", sequence); err != nil {
						if advanceCtx.Err() == nil {
							advanceErr = err
						}
						return
					}
				}
			}
		}()
		defer func() { cancel(); wg.Wait() }()
		inv := nativeDeterministicCase(t, p, ctx, policy, sre.TriggerSequence, "sequence "+name+".bounded_sequence", "explicit_maxvalue_limit")
		cancel()
		wg.Wait()
		if advanceErr != nil {
			t.Fatal(advanceErr)
		}
		results = append(results, incidentAcceptance{Fixture: "advancing_near_limit_sequence", Case: inv.ID,
			Detected: true, Diagnosed: true, Root: inv.Summary.Root, Reason: "manual_migration_only"})
	})
	var after int
	if err := p.QueryRow(ctx, "SELECT count(*) FROM sage.action_log").Scan(&after); err != nil || after != before {
		t.Fatalf("read-only incident cases changed action ledger: before=%d after=%d err=%v", before, after, err)
	}
	// The case evidence remains durable; the owned application objects and
	// sessions are removed. Deferred schema cleanup is independently logged.
	b, _ := json.MarshalIndent(struct {
		Policy  string               `json:"policy_hash"`
		Results []incidentAcceptance `json:"results"`
		Cleanup string               `json:"cleanup"`
	}{policy.Hash(), results, "unconditional session rollback and owned-schema drop"}, "", "  ")
	t.Log(string(b))
	writeSREReport(t, "PG_SAGE_SRE_INCIDENT_REPORT", b)
}
