package autonomy

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/ledger"
	"github.com/pg-sage/sidecar/internal/schemaguard"
)

// A retention dry run inside its 24h review parks with a dated reason in the
// decision ledger and does not stop the scan: a missing-FK invariant that
// follows it is still routed. Real PostgreSQL. No concurrent access tests:
// one custodian scans at a time per database (supervisor-serialized).

const reviewParkPrefix = "retention dry run in review until "

// orderedTestDetector returns this test's retention invariant before its
// missing-FK invariant, so the FK is the "later invariant" of the scan.
type orderedTestDetector struct {
	detector postgresSchemaDetector
	tables   map[string]bool
}

func (d orderedTestDetector) Detect(ctx context.Context) ([]schemaguard.Invariant, error) {
	appendItems, err := d.detector.detectUnboundedAppend(ctx)
	if err != nil {
		return nil, err
	}
	fkItems, err := d.detector.detectMissingFKIndexes(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]schemaguard.Invariant, 0, 2)
	for _, item := range append(appendItems, fkItems...) {
		if d.tables[item.Table] {
			result = append(result, item)
		}
	}
	return result, nil
}

func missingFKFixture(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	child := fmt.Sprintf("review_child_%d", time.Now().UnixNano())
	parent := child + "_parent"
	execAll(t, pool, "CREATE TABLE "+parent+" (id bigint PRIMARY KEY)",
		"CREATE TABLE "+child+" (id bigint, parent_id bigint REFERENCES "+parent+"(id))")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+child+", "+parent)
	})
	return child
}

func reviewParkCustodian(pool *pgxpool.Pool, router *recordingRouter, tables ...string,
) *schemaguard.Custodian {
	scope := map[string]bool{}
	for _, table := range tables {
		scope[table] = true
	}
	policyConfig := schemaguard.DefaultPolicy()
	policyConfig.AllowFKIndexApply, policyConfig.AllowRetentionApply = true, true
	return schemaguard.NewCustodian(
		orderedTestDetector{newPostgresSchemaDetector(pool, nil), scope},
		postgresSchemaContractSource{pool}, postgresSchemaHistorySource{pool},
		schemaRemediationRouter{database: "testdb", router: router,
			verifiedIndexes: router, retention: &postgresRetentionEnforcer{
				pool: pool, batchLimit: 10, pipeline: allowRetention}},
		schemaDecisionRecorder{ledger.NewService(ledger.NewPostgresRepository(pool))},
		policyConfig)
}

func routedTargets(router *recordingRouter, target string) int {
	count := 0
	for _, proposal := range router.routed() {
		for _, object := range proposal.TargetObjects {
			if object == target {
				count++
			}
		}
	}
	return count
}

func TestPendingRetentionDryRunParksAndScanContinues(t *testing.T) {
	pool := requireAutonomyDB(t)
	table := createdAtFixture(t, pool)
	child := missingFKFixture(t, pool)
	router := &recordingRouter{}
	custodian := reviewParkCustodian(pool, router, table, child)
	if _, err := custodian.Scan(context.Background()); err != nil {
		t.Fatalf("first Scan (dry run): %v", err)
	}

	result, err := custodian.Scan(context.Background())

	if err != nil {
		t.Fatalf("second Scan = %v; a dry run in review must park, not fail", err)
	}
	if result.Detected != 2 || result.Routed != 1 {
		t.Fatalf("second scan result=%+v, want the FK routed after the park", result)
	}
	if n := routedTargets(router, "public."+child); n != 2 {
		t.Fatalf("FK invariant routed %d times over two scans, want 2", n)
	}
	verdict, reason := latestSchemaDecision(t, pool, table)
	if verdict != string(ledger.VerdictPark) || !strings.HasPrefix(reason, reviewParkPrefix) {
		t.Fatalf("retention decision = %s %q, want park %q<time>", verdict, reason,
			reviewParkPrefix)
	}
	requireReviewDeadline(t, pool, strings.TrimPrefix(reason, reviewParkPrefix))
	requireNothingDeleted(t, pool, table, 3)
}

// requireReviewDeadline checks the reason's time is when the pending dry run
// leaves review: about 24h after it was recorded, by the database clock.
func requireReviewDeadline(t *testing.T, pool *pgxpool.Pool, text string) {
	t.Helper()
	until, err := time.Parse(time.RFC3339, text)
	if err != nil {
		t.Fatalf("review deadline %q is not RFC 3339: %v", text, err)
	}
	var dbNow time.Time
	if err := pool.QueryRow(context.Background(), "SELECT now()").Scan(&dbNow); err != nil {
		t.Fatalf("read database clock: %v", err)
	}
	lower, upper := dbNow.Add(retentionDryRunMinAge-time.Hour), dbNow.Add(retentionDryRunMinAge)
	if until.Before(lower) || until.After(upper) {
		t.Fatalf("review deadline %s outside [%s, %s]", until, lower, upper)
	}
}
