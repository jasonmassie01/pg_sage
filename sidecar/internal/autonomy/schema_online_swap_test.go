package autonomy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/ledger"
	"github.com/pg-sage/sidecar/internal/schemaguard"
)

// migration-09b: a type tightening's decision carries the online column
// swap script (or why there is none) next to the rewrite reference text.
// It stays evidence: no proposed SQL, never routed.

// No concurrent access test: one custodian records at a time per
// database (supervisor-serialized); the recorder holds no state.

func tighteningRecord(t *testing.T) schemaguard.DecisionRecord {
	t.Helper()
	return plannedRecord(t, schemaguard.Invariant{Kind: schemaguard.InvariantTypeTightening,
		Schema: "app", Table: "orders", Subject: "order_number"})
}

func TestOnlineSwapEvidenceIsRecordedWithTheRewrite(t *testing.T) {
	repository := &capturingRepository{}
	var calls atomic.Int32
	recorder := onlineSwapRecorder{ledger: ledger.NewService(repository),
		swap: func(_ context.Context, inv schemaguard.Invariant) map[string]any {
			calls.Add(1)
			return map[string]any{"online_swap_script": "-- script for " + inv.Subject}
		}}
	if err := recorder.Record(context.Background(), tighteningRecord(t)); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if calls.Load() != 1 || len(repository.inputs) != 1 {
		t.Fatalf("swap planned %d times, %d decisions recorded", calls.Load(),
			len(repository.inputs))
	}
	input := repository.inputs[0]
	if input.Evidence["online_swap_script"] != "-- script for order_number" ||
		input.Evidence["rewrite_statement"] == nil || input.ProposedSQL != "" ||
		input.Verdict != ledger.VerdictObserveOnly {
		t.Fatalf("decision input = %+v", input)
	}
}

// Evidence keys the swap returns never replace the decision's own.
func TestOnlineSwapEvidenceCannotOverwriteDecisionKeys(t *testing.T) {
	repository := &capturingRepository{}
	recorder := onlineSwapRecorder{ledger: ledger.NewService(repository),
		swap: func(context.Context, schemaguard.Invariant) map[string]any {
			return map[string]any{"route": "apply", "online_swap_script": "x"}
		}}
	if err := recorder.Record(context.Background(), tighteningRecord(t)); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if got := repository.inputs[0].Evidence["route"]; got != schemaguard.RouteRecommendation {
		t.Fatalf("route = %v, want the decision's recommendation route", got)
	}
}

// Nil and empty: no planner, or a planner with nothing to say, records
// the decision as migration-09 did.
func TestOnlineSwapEvidenceNilPlanner(t *testing.T) {
	for name, swap := range map[string]swapEvidenceFunc{
		"nil":   nil,
		"empty": func(context.Context, schemaguard.Invariant) map[string]any { return nil },
	} {
		repository := &capturingRepository{}
		recorder := onlineSwapRecorder{ledger: ledger.NewService(repository), swap: swap}
		if err := recorder.Record(context.Background(), tighteningRecord(t)); err != nil {
			t.Fatalf("%s: Record: %v", name, err)
		}
		if _, ok := repository.inputs[0].Evidence["online_swap_script"]; ok {
			t.Fatalf("%s: evidence %v", name, repository.inputs[0].Evidence)
		}
	}
}

// Only a type tightening with a column is planned; other invariants and
// a missing reader return nothing without a catalog read.
func TestPlanOnlineSwapEvidenceScope(t *testing.T) {
	ctx := context.Background()
	for _, inv := range []schemaguard.Invariant{
		{Kind: schemaguard.InvariantEverythingText, Schema: "app", Table: "orders"},
		{Kind: schemaguard.InvariantTypeTightening, Schema: "app", Table: "orders"},
	} {
		if got := planOnlineSwapEvidence(ctx, panicQuerier{}, inv); got != nil {
			t.Fatalf("%+v: evidence %v, want none", inv, got)
		}
	}
	inv := schemaguard.Invariant{Kind: schemaguard.InvariantTypeTightening, Schema: "app",
		Table: "orders", Subject: "order_number"}
	if got := planOnlineSwapEvidence(ctx, nil, inv); got != nil {
		t.Fatalf("nil reader: evidence %v", got)
	}
}

// Error propagation: a failed catalog read is recorded as the reason
// there is no script, not dropped.
func TestPlanOnlineSwapEvidenceReadFailure(t *testing.T) {
	inv := schemaguard.Invariant{Kind: schemaguard.InvariantTypeTightening, Schema: "app",
		Table: "orders", Subject: "order_number"}
	got := planOnlineSwapEvidence(context.Background(), failingQuerier{}, inv)
	reason, _ := got["online_swap_refused"].(string)
	if !strings.Contains(reason, "catalog unavailable") || got["online_swap_script"] != nil {
		t.Fatalf("evidence = %v, want the read failure as the refusal", got)
	}
}

// Integration: a text column of a keyed table gets a script; a table
// without a primary key gets the reason.
func TestPlanOnlineSwapEvidenceOnPostgres(t *testing.T) {
	pool := requireAutonomyDB(t)
	schemaName := fmt.Sprintf("mig09b_%d", time.Now().UnixNano())
	execAll(t, pool, "CREATE SCHEMA "+schemaName,
		"CREATE TABLE "+schemaName+".orders (id bigint PRIMARY KEY, order_number text)",
		"CREATE TABLE "+schemaName+".loose (order_number text)")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schemaName+" CASCADE")
	})
	reader := catalogread.New(pool, catalogread.Default())
	inv := schemaguard.Invariant{Kind: schemaguard.InvariantTypeTightening,
		Schema: schemaName, Table: "orders", Subject: "order_number"}
	got := planOnlineSwapEvidence(context.Background(), reader, inv)
	script, _ := got["online_swap_script"].(string)
	if !strings.Contains(script, "CREATE TRIGGER") || !strings.Contains(script, "recommend-only") ||
		got["online_swap_refused"] != nil {
		t.Fatalf("evidence = %v, want a script", got)
	}
	inv.Table = "loose"
	got = planOnlineSwapEvidence(context.Background(), reader, inv)
	if reason, _ := got["online_swap_refused"].(string); !strings.Contains(reason,
		"no primary key") {
		t.Fatalf("evidence = %v, want the no-primary-key refusal", got)
	}
}

// panicQuerier fails the test by panicking if anything reads the catalog.
type panicQuerier struct{}

func (panicQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) {
	panic("unexpected catalog read")
}

func (panicQuerier) QueryRow(context.Context, string, ...any) pgx.Row {
	panic("unexpected catalog read")
}

// failingQuerier fails every read.
type failingQuerier struct{}

var errCatalogUnavailable = errors.New("catalog unavailable")

func (failingQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errCatalogUnavailable
}

func (failingQuerier) QueryRow(context.Context, string, ...any) pgx.Row {
	return unavailableRow{}
}

type unavailableRow struct{}

func (unavailableRow) Scan(...any) error { return errCatalogUnavailable }
