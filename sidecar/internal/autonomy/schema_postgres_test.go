package autonomy

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/pg-sage/sidecar/internal/ledger"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/schemaguard"
)

type recordingLedgerRepository struct {
	inputs []ledger.DecisionInput
}

func (r *recordingLedgerRepository) InsertDecision(
	_ context.Context, input ledger.DecisionInput,
) (int64, error) {
	r.inputs = append(r.inputs, input)
	return int64(len(r.inputs)), nil
}

func (*recordingLedgerRepository) FindAuditViolations(
	context.Context,
) ([]ledger.AuditViolation, error) {
	return nil, nil
}

func TestPostgresSchemaGuardDetectsRoutesAndRecordsFKIndex(t *testing.T) {
	pool := requireAutonomyDB(t)
	ctx := context.Background()
	const parent = "autonomy_schema_parent"
	const child = "autonomy_schema_child"
	_, _ = pool.Exec(ctx, "DROP TABLE IF EXISTS "+child+", "+parent+" CASCADE")
	if _, err := pool.Exec(ctx, "CREATE TABLE "+parent+" (id bigint PRIMARY KEY); "+
		"CREATE TABLE "+child+" (id bigint PRIMARY KEY, parent_id bigint REFERENCES "+
		parent+"(id))"); err != nil {
		t.Fatalf("create schema guard fixture: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DROP TABLE IF EXISTS "+child+", "+parent+" CASCADE")
	})
	router := &recordingRouter{}
	repository := &recordingLedgerRepository{}
	guard, err := NewPostgresSchemaGuard(
		pool, "testdb", router, ledger.NewService(repository),
	)
	if err != nil {
		t.Fatalf("NewPostgresSchemaGuard: %v", err)
	}
	result, err := guard.Scan(ctx)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if result.Routed == 0 || result.Recorded == 0 {
		t.Fatalf("schema result = %#v", result)
	}
	if len(router.routed()) == 0 {
		t.Fatal("FK index was not routed through the standing-policy adapter")
	}
	if len(repository.inputs) == 0 || repository.inputs[0].Feature != "schema_guard" {
		t.Fatalf("schema decisions = %#v", repository.inputs)
	}
	if repository.inputs[0].Verdict != ledger.VerdictObserveOnly ||
		!strings.Contains(repository.inputs[0].ProposedSQL, "CREATE INDEX CONCURRENTLY") {
		t.Fatalf("schema recommendation = %#v", repository.inputs[0])
	}
}

func TestPostgresSchemaSourcesUseTableContractAndDryRunHistory(t *testing.T) {
	pool := requireAutonomyDB(t)
	ctx := context.Background()
	table := fmt.Sprintf("autonomy_append_%d", time.Now().UnixNano())
	if _, err := pool.Exec(ctx, "CREATE TABLE "+table+" (id bigint)"); err != nil {
		t.Fatalf("create append fixture: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.table_contract
		(schema_name, table_name, append_only, retention_interval,
		 declared_by, evidence_id) VALUES ('public',$1,true,interval '30 days','test',$2)`,
		table, "contract_"+table); err != nil {
		t.Fatalf("insert table contract: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.table_contract WHERE table_name=$1", table)
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+table)
	})
	invariant := schemaguard.Invariant{
		Kind: schemaguard.InvariantUnboundedAppend, Schema: "public", Table: table,
	}
	contract, err := (postgresSchemaContractSource{pool}).Contract(ctx, invariant)
	if err != nil || !contract.AppendOnly || contract.RetentionWindow != 30*24*time.Hour {
		t.Fatalf("table contract=%#v err=%v", contract, err)
	}
	invariants, err := (postgresSchemaDetector{pool}).Detect(ctx)
	if err != nil || !containsInvariant(invariants, invariant) {
		t.Fatalf("append invariant missing from %#v err=%v", invariants, err)
	}
	historySource := postgresSchemaHistorySource{pool}
	history, err := historySource.History(ctx, invariant)
	if err != nil || history.SuccessfulRetentionDryRuns != 0 {
		t.Fatalf("initial history=%#v err=%v", history, err)
	}
	decision, err := ledger.NewService(ledger.NewPostgresRepository(pool)).RecordDecision(
		ctx, ledger.DecisionInput{
			Feature: "schema_guard", Intent: string(invariant.Kind),
			Evidence: map[string]any{"disposition": "dry_run"},
			Verdict:  ledger.VerdictObserveOnly, Reason: "retention dry run",
			RiskTier: "moderate", PolicyVersion: 1,
			TargetObjects: []string{"public." + table},
		},
	)
	if err != nil {
		t.Fatalf("record dry run: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM sage.decision WHERE id=$1", decision.ID)
	})
	history, err = historySource.History(ctx, invariant)
	if err != nil || history.SuccessfulRetentionDryRuns != 1 {
		t.Fatalf("dry-run history=%#v err=%v", history, err)
	}
}

func containsInvariant(items []schemaguard.Invariant, target schemaguard.Invariant) bool {
	for _, item := range items {
		if item.Kind == target.Kind && item.Schema == target.Schema && item.Table == target.Table {
			return true
		}
	}
	return false
}

func TestSchemaRemediationRouterBuildsTypedFKProposal(t *testing.T) {
	router := &recordingRouter{}
	adapter := schemaRemediationRouter{database: "orders", router: router}
	err := adapter.Route(context.Background(), schemaguard.Remediation{
		Invariant: schemaguard.Invariant{
			Schema: "public", Table: "orders",
			ProposedSQL: "CREATE INDEX CONCURRENTLY idx ON public.orders(customer_id)",
		},
	})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	got := router.routed()
	if len(got) != 1 || got[0].Feature != string(policy.ChangeFKIndex) ||
		got[0].Database != "orders" {
		t.Fatalf("typed FK proposal = %#v", got)
	}
	if err := adapter.Route(context.Background(), schemaguard.Remediation{}); err == nil {
		t.Fatal("empty schema remediation was routed")
	}
}

func TestPostgresSchemaGuardRejectsIncompleteDependencies(t *testing.T) {
	if _, err := NewPostgresSchemaGuard(nil, "orders", nil, nil); err == nil {
		t.Fatal("incomplete schema guard dependencies were accepted")
	}
}

func TestBoundedIdentifierAvoidsCollisionsAndBrokenUTF8(t *testing.T) {
	prefix := strings.Repeat("a", 70)
	left := boundedIdentifier(prefix + "left")
	right := boundedIdentifier(prefix + "right")
	if left == right || len(left) > 63 || len(right) > 63 {
		t.Fatalf("bounded identifiers collide or exceed limit: %q %q", left, right)
	}
	unicodeName := boundedIdentifier(strings.Repeat("schema_😀", 12))
	if len(unicodeName) > 63 || !utf8.ValidString(unicodeName) {
		t.Fatalf("bounded Unicode identifier = %q (%d bytes)", unicodeName, len(unicodeName))
	}
}
