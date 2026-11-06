package executor

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/facts"
	"github.com/pg-sage/sidecar/internal/policy"
)

// facts-01: a confirmed test-fixture fact must not hold back configuration
// findings, whose "instance" target names no relation.

// No concurrent access tests here: findingRequest builds a fresh request,
// and the binder's concurrency is covered in facts/binder_targets_test.go.

type confirmedFacts []facts.Fact

func (c confirmedFacts) Confirmed(context.Context) ([]facts.Fact, error) { return c, nil }

// failingTargets resolves index refs as written but cannot read targets.
type failingTargets struct{ err error }

func (f failingTargets) ResolveIndexes(_ context.Context, refs []facts.ObjectRef) (
	[]facts.ObjectRef, error) {
	return refs, nil
}

func (f failingTargets) TargetSchemas(context.Context, []string) (map[string][]string,
	error) {
	return nil, f.err
}

func fixtureFact() confirmedFacts {
	at := time.Now().Add(-time.Hour)
	return confirmedFacts{{ID: 9, Type: facts.TypeTestFixture, Kind: facts.KindSchema,
		Subject: "test_*", Status: facts.StatusConfirmed, DecidedBy: "op", DecidedAt: &at}}
}

func configFinding(sql string) analyzer.Finding {
	return analyzer.Finding{Category: "memory_tuning", ObjectType: "configuration",
		ObjectIdentifier: "instance", RecommendedSQL: sql}
}

func TestConfigFindingWithFixtureFactReachesApprovalQueue(t *testing.T) {
	binder := facts.NewBinder(fixtureFact(), nil, nil)
	exec := factBoundExecutor(true, binder)
	got := exec.evaluateFindingPolicy(context.Background(),
		configFinding("ALTER SYSTEM SET jit = off"), false)
	if got.Decision != PolicyDecisionQueueApproval || !got.RequiresApproval {
		t.Fatalf("config finding with a fixture fact: %+v, want queue_approval", got)
	}
	// A reload-only setting gets the decision it gets without facts.
	plain := factBoundExecutor(true, nil).evaluateFindingPolicy(context.Background(),
		configFinding("ALTER SYSTEM SET work_mem = '64MB'"), false)
	bound := exec.evaluateFindingPolicy(context.Background(),
		configFinding("ALTER SYSTEM SET work_mem = '64MB'"), false)
	if bound.BlockedReason == string(policy.ReasonBoundByFact) ||
		bound.Decision != plain.Decision || bound.BlockedReason != plain.BlockedReason {
		t.Fatalf("work_mem with fact %+v, without %+v", bound, plain)
	}
}

func TestFixtureFactStillBindsTableFindingsInItsSchema(t *testing.T) {
	exec := factBoundExecutor(true, facts.NewBinder(fixtureFact(), nil, nil))
	got := exec.evaluateFindingPolicy(context.Background(), analyzer.Finding{
		Category: "stale_statistics", ObjectType: "table",
		ObjectIdentifier: "test_a.orders", RecommendedSQL: "ANALYZE test_a.orders"}, false)
	if got.Decision != PolicyDecisionBlocked ||
		got.BlockedReason != string(policy.ReasonBoundByFact) {
		t.Fatalf("fixture table finding: %+v, want blocked by fact", got)
	}
}

func TestTargetResolutionErrorFailsTheGateClosed(t *testing.T) {
	boom := errors.New("connection refused")
	binder := facts.NewBinder(fixtureFact(), failingTargets{err: boom}, nil)
	exec := factBoundExecutor(true, binder)
	got := exec.evaluateFindingPolicy(context.Background(), analyzer.Finding{
		Category: "stale_statistics", ObjectType: "table", ObjectIdentifier: "orders",
		RecommendedSQL: "ANALYZE public.orders"}, false)
	if got.Decision != PolicyDecisionBlocked ||
		got.BlockedReason != string(policy.ReasonFactsUnavailable) {
		t.Fatalf("unreadable targets: %+v, want blocked facts_unavailable", got)
	}
}

func TestTargetObjectsForFindingSkipsNonRelationTypes(t *testing.T) {
	cases := []struct {
		objectType, id string
		want           []string
	}{
		{"configuration", "instance", nil},
		{"configuration", "instance:work_mem", nil},
		{"setting", "work_mem", nil},
		{"database", "lifeos", nil},
		{"instance", "instance", nil},
		{"query", "1234567890", nil},
		{"Configuration", "instance", nil},
		{"configuration", "slot:cdc", []string{"slot:cdc"}},
		{"replication_slot", "slot:cdc", []string{"slot:cdc"}},
		{"table", "public.orders", []string{"public.orders"}},
		{"index", " public.idx_a ", []string{"public.idx_a"}},
		{"", "public.orders", []string{"public.orders"}},
		{"advisor", "llm", []string{"llm"}},
		{"table", "  ", nil},
		{"configuration", "", nil},
	}
	for _, c := range cases {
		got := targetObjectsForFinding(analyzer.Finding{ObjectType: c.objectType,
			ObjectIdentifier: c.id})
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s %q: targets %v, want %v", c.objectType, c.id, got, c.want)
		}
	}
}
