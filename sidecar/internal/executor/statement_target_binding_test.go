package executor

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/optimizer"
)

func bindingExecutor() *Executor {
	exec := New(nil, &config.Config{}, time.Time{}, nopLog)
	exec.WithPolicyGate(fullyEnabledGate(time.Now()))
	return exec
}

func boundVacuumFinding(ident, sql string) analyzer.Finding {
	return analyzer.Finding{Category: "table_bloat", ObjectType: "table",
		ObjectIdentifier: ident, RecommendedSQL: sql, ActionRisk: "safe"}
}

// executor-08: an autonomous finding that names public.a but whose
// statement changes public.b is refused as target_mismatch; the control
// (the statement on the named table) executes.
func TestFindingTargetMismatchRefused(t *testing.T) {
	exec := bindingExecutor()
	ctx := context.Background()
	refused := exec.evaluateFindingPolicy(ctx,
		boundVacuumFinding("public.a", "VACUUM public.b;"), false)
	if refused.Decision != PolicyDecisionBlocked || refused.BlockedReason != "target_mismatch" {
		t.Fatalf("decision = %s/%s, want blocked/target_mismatch",
			refused.Decision, refused.BlockedReason)
	}
	if !strings.Contains(refused.Detail, "public.a") ||
		!strings.Contains(refused.Detail, "public.b") {
		t.Fatalf("detail %q must name both the finding's and the statement's target",
			refused.Detail)
	}
	control := exec.evaluateFindingPolicy(ctx,
		boundVacuumFinding("public.a", "VACUUM public.a;"), false)
	if control.Decision != PolicyDecisionExecute {
		t.Fatalf("control decision = %s/%s, want execute", control.Decision,
			control.BlockedReason)
	}
}

// A refused mismatch never reaches the lease (nil pool: a lease attempt
// would fail with "database pool unavailable") nor the statement.
func TestTargetMismatchTakesNoLease(t *testing.T) {
	exec := bindingExecutor()
	f := boundVacuumFinding("public.a", "VACUUM public.b;")
	executed := false
	_, err := exec.Apply(context.Background(), ActionIntent{
		Request: findingRequest(f, false), Lease: &f,
		Execute: func(context.Context, ActionPolicyDecision) (int64, error) {
			executed = true
			return 1, nil
		},
	})
	var withheld *WithheldError
	if !errors.As(err, &withheld) {
		t.Fatalf("Apply error = %v, want a *WithheldError", err)
	}
	if withheld.Decision.BlockedReason != "target_mismatch" || withheld.Reauthorized {
		t.Fatalf("withheld = %+v, want target_mismatch at the first authorization",
			withheld.Decision)
	}
	if executed || strings.Contains(err.Error(), "pool") {
		t.Fatalf("mismatch went past authorization (executed=%v, err=%v)", executed, err)
	}
}

// Identifier spelling: default schema public and identifier folding make
// the same relation match; another schema, name or case does not.
func TestFindingTargetSpellings(t *testing.T) {
	exec := bindingExecutor()
	cases := []struct {
		ident, sql string
		mismatch   bool
	}{
		{"public.Orders", `VACUUM "public"."Orders"`, false},
		{"orders", "VACUUM public.orders", false},
		{"public.orders", "VACUUM orders", false},
		{`"public"."orders"`, "VACUUM PUBLIC.ORDERS", false},
		{"public.orders", `VACUUM (FREEZE) "public"."orders";`, false},
		{"public.orders(customer_id)", "VACUUM public.orders", false},
		{`public."a""b"`, `VACUUM public."a""b"`, false},
		{`public.""`, `VACUUM public.x`, true},
		{"public.", `VACUUM public.x`, true},
		// Fails closed: the statement parser cuts a name at "(".
		{`public."Odd(name)"`, `VACUUM public."Odd(name)"`, true},
		{`public."Orders"`, "VACUUM public.orders", true},
		{"sales.orders", "VACUUM public.orders", true},
		{"public.orders", "VACUUM public.orders_archive", true},
		{"public.orders", `VACUUM "public.orders"`, true},
		{`"public.orders`, "VACUUM public.orders", true},
		{"a.b.c", "VACUUM b.c", true},
	}
	for _, tc := range cases {
		d := exec.evaluateFindingPolicy(context.Background(),
			boundVacuumFinding(tc.ident, tc.sql), false)
		got := d.BlockedReason == "target_mismatch"
		if got != tc.mismatch {
			t.Errorf("ident %q sql %q: decision %s/%s, mismatch=%v want %v",
				tc.ident, tc.sql, d.Decision, d.BlockedReason, got, tc.mismatch)
		}
	}
}

// The exported binding reports a typed error naming the reason and both
// objects (the catalog parity test and callers read it).
func TestBindFindingTargetError(t *testing.T) {
	_, err := BindFindingTarget(boundVacuumFinding("public.a", "VACUUM public.b"))
	var mismatch *TargetMismatchError
	if !errors.As(err, &mismatch) || mismatch.Named != "public.a" ||
		mismatch.Statement != "public.b" {
		t.Fatalf("BindFindingTarget error = %#v, want a TargetMismatchError", err)
	}
	if !strings.HasPrefix(err.Error(), "target_mismatch: ") {
		t.Fatalf("error %q does not lead with the reason", err)
	}
	binding, err := BindFindingTarget(boundVacuumFinding("public.a", "VACUUM public.a"))
	if err != nil || binding.Statement != "public.a" || binding.Named != "public.a" ||
		!reflect.DeepEqual(binding.Targets, []string{"public.a"}) {
		t.Fatalf("matched binding = %+v, %v", binding, err)
	}
}

// An index finding binds to the index its DROP names.
func TestIndexFindingTargetMismatch(t *testing.T) {
	f := analyzer.Finding{Category: "unused_index", ObjectType: "index",
		ObjectIdentifier: "public.idx_a", RecommendedSQL: "DROP INDEX CONCURRENTLY public.idx_b;"}
	d := bindingExecutor().evaluateFindingPolicy(context.Background(), f, false)
	if d.BlockedReason != "target_mismatch" {
		t.Fatalf("DROP of another index: %s/%s, want target_mismatch", d.Decision, d.BlockedReason)
	}
}

// Nil/empty: no identifier binds the request and its lease to the
// statement's own target; empty SQL has no contract.
func TestEmptyIdentifierUsesStatementTarget(t *testing.T) {
	f := boundVacuumFinding("", `VACUUM "public"."orders";`)
	request := findingRequest(f, false)
	if want := []string{`"public"."orders"`}; !reflect.DeepEqual(request.TargetObjs, want) {
		t.Fatalf("TargetObjs = %q, want the statement target %q", request.TargetObjs, want)
	}
	spec, ok := leaseSpecFor(ActionIntent{Request: request, Lease: &f})
	if !ok || !reflect.DeepEqual(spec.Targets, []string{`"public"."orders"`}) {
		t.Fatalf("lease = %+v (ok=%v), want the statement target", spec, ok)
	}
	empty := bindingExecutor().evaluateFindingPolicy(context.Background(),
		boundVacuumFinding("public.orders", ""), false)
	if empty.Decision != PolicyDecisionParked || empty.BlockedReason != "no_typed_contract" {
		t.Fatalf("empty SQL: %s/%s, want parked/no_typed_contract",
			empty.Decision, empty.BlockedReason)
	}
}

// A finding whose identifier names no relation (a setting, the instance)
// is not compared: its request keeps today's targets.
func TestNonRelationFindingNotBound(t *testing.T) {
	f := analyzer.Finding{Category: "vacuum_tuning", ObjectType: "configuration",
		ObjectIdentifier: "instance",
		RecommendedSQL:   "ALTER TABLE public.t SET (autovacuum_vacuum_scale_factor = 0.05);"}
	request := findingRequest(f, false)
	if !reflect.DeepEqual(request.TargetObjs, []string{"instance"}) {
		t.Fatalf("TargetObjs = %q, want [instance] (unchanged)", request.TargetObjs)
	}
	d := bindingExecutor().evaluateFindingPolicy(context.Background(), f, false)
	if d.BlockedReason == "target_mismatch" {
		t.Fatalf("configuration finding refused as target_mismatch: %+v", d)
	}
}

// An untyped finding (no object type) keeps today's request: only a
// finding that declares a relation is compared.
func TestUntypedFindingNotBound(t *testing.T) {
	f := analyzer.Finding{ObjectIdentifier: "public.t",
		RecommendedSQL: "DROP INDEX CONCURRENTLY public.idx_x;"}
	request := findingRequest(f, false)
	if request.TargetMismatch != "" ||
		!reflect.DeepEqual(request.TargetObjs, []string{"public.t"}) {
		t.Fatalf("untyped finding request = %q / %q, want unbound [public.t]",
			request.TargetMismatch, request.TargetObjs)
	}
}

// Optimizer findings compare the table part of their identifier.
func TestOptimizerFindingBindsTable(t *testing.T) {
	ok := analyzer.OptimizerRecommendationFinding(optimizer.Recommendation{
		Table: "public.orders", Severity: "warning",
		DDL: "CREATE INDEX CONCURRENTLY idx_o ON public.orders (customer_id)"})
	if !strings.Contains(ok.ObjectIdentifier, "|") {
		t.Fatalf("fixture identifier %q lacks its index fingerprint", ok.ObjectIdentifier)
	}
	exec := bindingExecutor()
	if d := exec.evaluateFindingPolicy(context.Background(), ok, false); d.BlockedReason ==
		"target_mismatch" {
		t.Fatalf("optimizer finding on its own table refused: %+v", d)
	}
	bad := ok
	bad.RecommendedSQL = "CREATE INDEX CONCURRENTLY idx_o ON public.customers (customer_id)"
	if d := exec.evaluateFindingPolicy(context.Background(), bad, false); d.BlockedReason !=
		"target_mismatch" {
		t.Fatalf("optimizer DDL on another table: %s/%s, want target_mismatch",
			d.Decision, d.BlockedReason)
	}
}

// Custodian proposals bind their statement to their target objects.
func TestCustodianTargetBinding(t *testing.T) {
	exec := bindingExecutor()
	ctx := context.Background()
	bad := CustodianProposal{Feature: "freeze", SQL: `VACUUM (FREEZE) "public"."b"`,
		TargetObjects: []string{"public.a"}}
	if d := exec.EvaluateCustodianProposal(ctx, bad); d.BlockedReason != "target_mismatch" {
		t.Fatalf("custodian mismatch: %s/%s, want target_mismatch", d.Decision, d.BlockedReason)
	}
	good := bad
	good.TargetObjects = []string{"public.b"}
	if d := exec.EvaluateCustodianProposal(ctx, good); d.BlockedReason == "target_mismatch" {
		t.Fatalf("custodian on its own target refused: %+v", d)
	}
	empty := bad
	empty.TargetObjects = nil
	request := custodianRequest(empty)
	if !reflect.DeepEqual(request.TargetObjs, []string{`"public"."b"`}) {
		t.Fatalf("custodian without targets: TargetObjs = %q, want the statement's",
			request.TargetObjs)
	}
}

// A rollback re-runs pg_sage's recorded inverse, which names another
// object than the forward finding (the table of a dropped index): it is
// never refused as target_mismatch.
func TestRollbackNotTargetBound(t *testing.T) {
	f := analyzer.Finding{Category: "unused_index", ObjectType: "index",
		ObjectIdentifier: "public.idx_a",
		RecommendedSQL:   "CREATE INDEX CONCURRENTLY idx_a ON public.t (a)"}
	d := bindingExecutor().evaluateRollbackPolicy(context.Background(), f, 0)
	if d.BlockedReason == "target_mismatch" {
		t.Fatalf("rollback refused as target_mismatch: %+v", d)
	}
}

// findingRequest is a pure function of its finding: concurrent callers
// get the same binding.
func TestFindingRequestBindingConcurrent(t *testing.T) {
	f := boundVacuumFinding("", `VACUUM "public"."orders";`)
	var wg sync.WaitGroup
	results := make([][]string, 16)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = findingRequest(f, false).TargetObjs
		}(i)
	}
	wg.Wait()
	for i, got := range results {
		if !reflect.DeepEqual(got, []string{`"public"."orders"`}) {
			t.Fatalf("goroutine %d: TargetObjs = %q", i, got)
		}
	}
}
