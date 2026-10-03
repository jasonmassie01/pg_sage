package autonomy

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/schemaguard"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// The v1.8.1 ledger flood (dogfood lifeos, v1.8.3): 252,974 schema guard
// rows naming ~160 leaked test_* schemas, up to 71 per row. Every row
// matched the scan's targets, and the history read re-aggregated all of
// them on every structural scan: 12.7 s a call. The read must cost the
// same however large the legacy ledger is.

const (
	floodSchemas       = 160
	floodTables        = 17
	floodKeys          = 170 // floodTables tables x 10 subjects
	floodTargetsPerRow = 70
	floodRows          = 250000
	floodScanBudget    = 200 * time.Millisecond
	floodRowsReadLimit = 1000
)

// floodLedger is the seeded legacy ledger and what History must return.
type floodLedger struct {
	invariants []schemaguard.Invariant
	lastHash   map[string]string
	dryRuns    map[schemaguard.HistoryKey]int
	reversions map[schemaguard.HistoryKey]int
}

func TestHistoryReadIsFlatOnALegacyFloodLedger(t *testing.T) {
	if testing.Short() {
		t.Skip("250,000-row legacy ledger skipped in -short mode")
	}
	ctx := context.Background()
	pool, recorder := floodPool(t)
	ledgerRows := seedFloodLedger(t, ctx, pool)
	source := postgresSchemaHistorySource{pool}
	index, err := source.History(ctx, ledgerRows.invariants)
	if err != nil {
		t.Fatalf("History on the flood ledger: %v", err)
	}
	requireFloodHistory(t, index, ledgerRows)
	best := bestOf(t, 3, func() error {
		_, err := source.History(ctx, ledgerRows.invariants)
		return err
	})
	if best > floodScanBudget {
		t.Fatalf("history read on %d legacy rows took %v, want < %v", floodRows, best,
			floodScanBudget)
	}
	t.Logf("history read on %d legacy rows: best of 3 %v", floodRows, best)
	recorder.Reset()
	if _, err := source.History(ctx, ledgerRows.invariants); err != nil {
		t.Fatalf("History (recorded): %v", err)
	}
	requireBoundedHistoryPlan(t, ctx, pool, recorder)
	requireFastFloodScan(t, ctx, pool, ledgerRows)
}

// floodPool is a freshly bootstrapped database of its own (the flood must
// not slow the package's other tests) whose statements are recorded.
func floodPool(t *testing.T) (*pgxpool.Pool, *testdb.QueryRecorder) {
	t.Helper()
	dsn := testdb.CreateDatabase(t, "guardflood")
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse flood DSN: %v", err)
	}
	recorder := &testdb.QueryRecorder{}
	config.ConnConfig.Tracer = recorder
	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatalf("connect flood database: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(context.Background(), pool); err != nil {
		t.Fatalf("bootstrap flood database: %v", err)
	}
	return pool, recorder
}

// floodFamily is the leaked clone family: floodSchemas idle copies.
func floodFamily() *schemaguard.Family {
	members := make([]string, floodSchemas)
	for i := range members {
		members[i] = fmt.Sprintf("test_%08x", 0x5a000000+i*7919)
	}
	return &schemaguard.Family{Key: "test_*:5eed", Members: members, Idle: true,
		Reason: "no scans, writes, statements or sessions"}
}

func floodKind(key int) schemaguard.InvariantKind {
	switch {
	case key < 110:
		return schemaguard.InvariantTypeTightening
	case key < 150:
		return schemaguard.InvariantEverythingText
	default:
		return schemaguard.InvariantMissingFKIndex
	}
}

// floodInvariant is key's invariant on one member schema.
func floodInvariant(family *schemaguard.Family, key int, schemaName string) schemaguard.Invariant {
	return schemaguard.Invariant{Kind: floodKind(key), Schema: schemaName,
		Table: fmt.Sprintf("t_%02d", key%floodTables), Subject: fmt.Sprintf("c%d",
			key/floodTables), Family: family}
}

// seedFloodLedger writes floodRows legacy rows (set-based), then the
// newest row of every identity, retention dry runs and external reversions.
func seedFloodLedger(t *testing.T, ctx context.Context, pool *pgxpool.Pool) floodLedger {
	t.Helper()
	family := floodFamily()
	out := floodLedger{lastHash: map[string]string{},
		dryRuns: map[schemaguard.HistoryKey]int{}, reversions: map[schemaguard.HistoryKey]int{}}
	keys, intents, tables := make([]string, 0, floodKeys), make([]string, 0, floodKeys),
		make([]string, 0, floodKeys)
	for key := range floodKeys {
		for _, member := range family.Members {
			out.invariants = append(out.invariants, floodInvariant(family, key, member))
		}
		identity := schemaguard.InvariantIdentity(floodInvariant(family, key, family.Members[0]))
		keys, intents = append(keys, identity), append(intents, string(floodKind(key)))
		tables = append(tables, fmt.Sprintf("t_%02d", key%floodTables))
		out.lastHash[identity] = fmt.Sprintf("latest-%d", key)
	}
	start := time.Now()
	if _, err := pool.Exec(ctx, floodSeedSQL, keys, intents, tables, family.Members,
		floodRows, floodTargetsPerRow); err != nil {
		t.Fatalf("seed legacy flood: %v", err)
	}
	if _, err := pool.Exec(ctx, floodLatestSQL, keys, intents, tables,
		family.Members); err != nil {
		t.Fatalf("seed newest rows: %v", err)
	}
	seedFloodCounts(t, ctx, pool, family, &out)
	if _, err := pool.Exec(ctx, "VACUUM (ANALYZE) sage.decision"); err != nil {
		t.Fatalf("analyze flood ledger: %v", err)
	}
	t.Logf("seeded %d legacy rows in %v", floodRows, time.Since(start).Round(time.Second))
	return out
}

// floodSeedSQL: row g belongs to identity g % keys and lists $6 member
// schemas of its table, in one of two windows (the family's membership
// drifted between scans, as test schemas came and went).
const floodSeedSQL = `
WITH k AS (
    SELECT ord - 1 AS n, key, intent, tbl
    FROM unnest($1::text[], $2::text[], $3::text[]) WITH ORDINALITY AS u(key, intent, tbl, ord)
), s AS (
    SELECT ord - 1 AS n, name FROM unnest($4::text[]) WITH ORDINALITY AS u(name, ord)
), windows AS (
    SELECT k.n, w.w, jsonb_agg(s.name || '.' || k.tbl ORDER BY s.n) AS targets
    FROM k CROSS JOIN generate_series(0, 1) AS w(w)
    JOIN s ON ((s.n - (k.n * 7 + w.w * 45)) % cardinality($4::text[])
               + cardinality($4::text[])) % cardinality($4::text[]) < $6::int
    GROUP BY k.n, w.w
)
INSERT INTO sage.decision (feature, intent, target_objects, verdict, risk_tier, reason,
    evidence, evidence_id, created_at)
SELECT 'schema_guard', k.intent, windows.targets,
       CASE WHEN k.intent = 'missing_fk_index' THEN 'parked' ELSE 'observe_only' END,
       'moderate', 'legacy flood',
       jsonb_build_object('disposition', CASE WHEN k.intent = 'missing_fk_index'
           THEN 'park' ELSE 'recommend' END, 'invariant_key', k.key,
           'decision_hash', 'flood-' || k.n),
       'guardflood-' || g, now() - interval '20 hours' + g * interval '200 ms'
FROM generate_series(0, $5::int - 1) AS g
JOIN k ON k.n = g % cardinality($1::text[])
JOIN windows ON windows.n = k.n AND windows.w = (g / cardinality($1::text[])) % 2`

// floodLatestSQL records the newest decision of every identity.
const floodLatestSQL = `
INSERT INTO sage.decision (feature, intent, target_objects, verdict, risk_tier, reason,
    evidence, evidence_id)
SELECT 'schema_guard', u.intent, jsonb_build_array(($4::text[])[1] || '.' || u.tbl),
       'observe_only',
       'moderate', 'latest decision',
       jsonb_build_object('disposition', 'recommend', 'invariant_key', u.key,
           'decision_hash', 'latest-' || (u.ord - 1)),
       'guardlatest-' || u.ord
FROM unnest($1::text[], $2::text[], $3::text[]) WITH ORDINALITY AS u(key, intent, tbl, ord)`

// seedFloodCounts writes the rows History counts: retention dry runs of
// an unbounded-append family table (row j lists members 0..j) and two
// external reversions of one type-tightening member.
func seedFloodCounts(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, family *schemaguard.Family,
	out *floodLedger,
) {
	t.Helper()
	appendInvariant := schemaguard.Invariant{Kind: schemaguard.InvariantUnboundedAppend,
		Schema: family.Members[0], Table: "t_00", Family: family}
	identity := schemaguard.InvariantIdentity(appendInvariant)
	const dryRunRows = 6
	for j := range dryRunRows {
		targets := make([]string, 0, j+1)
		for i := 0; i <= j; i++ {
			targets = append(targets, family.Members[i]+".t_00")
		}
		insertFloodCountRow(t, ctx, pool, string(appendInvariant.Kind), targets,
			map[string]any{"disposition": "dry_run", "invariant_key": identity,
				"decision_hash": fmt.Sprintf("ua-%d", j)})
	}
	for i := range dryRunRows {
		member := appendInvariant
		member.Schema = family.Members[i]
		out.invariants = append(out.invariants, member)
		out.dryRuns[schemaguard.HistoryKey{Kind: member.Kind,
			Target: member.Target()}] = dryRunRows - i
	}
	out.lastHash[identity] = fmt.Sprintf("ua-%d", dryRunRows-1)
	reverted := floodInvariant(family, 0, family.Members[0])
	for range 2 {
		insertFloodCountRow(t, ctx, pool, string(reverted.Kind),
			[]string{reverted.Target()}, map[string]any{"disposition": "recommend",
				"external_reversion": "true"})
	}
	out.reversions[schemaguard.HistoryKey{Kind: reverted.Kind, Target: reverted.Target()}] = 2
}

func insertFloodCountRow(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, intent string,
	targets []string, evidence map[string]any,
) {
	t.Helper()
	if _, err := pool.Exec(ctx, `INSERT INTO sage.decision (feature, intent,
		target_objects, verdict, risk_tier, reason, evidence, evidence_id)
		VALUES ('schema_guard', $1, to_jsonb($2::text[]), 'observe_only', 'moderate',
		'counted', $3, 'guardcount-' || gen_random_uuid())`, intent, targets,
		evidence); err != nil {
		t.Fatalf("seed counted row: %v", err)
	}
}

// requireFloodHistory checks History returned exactly the seeded truth.
func requireFloodHistory(t *testing.T, index schemaguard.HistoryIndex, want floodLedger) {
	t.Helper()
	if len(index.LastHash) != len(want.lastHash) {
		t.Fatalf("history returned %d identities, want %d", len(index.LastHash),
			len(want.lastHash))
	}
	for identity, hash := range want.lastHash {
		if got := index.LastHash[identity]; got != hash {
			t.Fatalf("last hash of %s = %q, want the newest row's %q", identity, got, hash)
		}
	}
	for _, invariant := range want.invariants {
		key := schemaguard.HistoryKey{Kind: invariant.Kind, Target: invariant.Target()}
		wantHistory := schemaguard.History{SuccessfulRetentionDryRuns: want.dryRuns[key],
			ExternalReversions: want.reversions[key]}
		if got := index.For(invariant); got != wantHistory {
			t.Fatalf("history of %s %s = %+v, want %+v", invariant.Kind,
				invariant.Target(), got, wantHistory)
		}
	}
}

// requireBoundedHistoryPlan explains the statement History ran, with its
// arguments, and executes it in one transaction: sage.decision is read
// through an index only, and the rows read do not grow with the ledger.
func requireBoundedHistoryPlan(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, recorder *testdb.QueryRecorder,
) {
	t.Helper()
	recorded := recorder.Matching("sage.decision")
	if len(recorded) != 1 {
		t.Fatalf("History ran %d statements on sage.decision, want 1", len(recorded))
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	// The counters include the connection's earlier statements not yet
	// flushed: only the delta around the explain is this read's.
	before, err := testdb.XactScansOf(ctx, tx, "sage.decision")
	if err != nil {
		t.Fatalf("read scan counters: %v", err)
	}
	plan, err := testdb.Explain(ctx, tx, "ANALYZE", recorded[0].SQL, recorded[0].Args...)
	if err != nil {
		t.Fatalf("explain history read: %v", err)
	}
	if plan.SeqScans("decision") != 0 || !plan.Has(func(n testdb.PlanNode) bool {
		return strings.HasPrefix(n.Index, "idx_decision_")
	}) {
		t.Fatalf("history read does not use an index on sage.decision:\n%s", plan)
	}
	rows := 0.0
	plan.Walk(func(n testdb.PlanNode) {
		if n.Relation == "decision" {
			rows += n.ActualRows * max(n.ActualLoops, 1)
		}
	})
	after, err := testdb.XactScansOf(ctx, tx, "sage.decision")
	if err != nil {
		t.Fatalf("read scan counters: %v", err)
	}
	seqRead, fetched := after.SeqRead-before.SeqRead, after.IndexFetch-before.IndexFetch
	if rows > floodRowsReadLimit || seqRead != 0 || fetched > floodRowsReadLimit {
		t.Fatalf("history read %g plan rows, %d seq rows, %d index fetches of %d legacy "+
			"rows, want <= %d:\n%s", rows, seqRead, fetched, floodRows,
			floodRowsReadLimit, plan)
	}
	t.Logf("history read: %g plan rows, %d index fetches", rows, fetched)
}

// requireFastFloodScan runs whole guard scans (the real contract and
// history sources) over the flood ledger's invariants.
func requireFastFloodScan(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, ledgerRows floodLedger,
) {
	t.Helper()
	recorder := &floodRecorder{}
	guard := schemaguard.NewCustodian(floodDetector(ledgerRows.invariants),
		postgresSchemaContractSource{pool}, postgresSchemaHistorySource{pool},
		floodRouter{}, recorder, schemaguard.DefaultPolicy())
	best := bestOf(t, 3, func() error {
		result, err := guard.Scan(ctx)
		if err == nil && result.Detected != len(ledgerRows.invariants) {
			err = fmt.Errorf("scan detected %d, want %d", result.Detected,
				len(ledgerRows.invariants))
		}
		return err
	})
	if best > floodScanBudget {
		t.Fatalf("schema guard scan over %d legacy rows took %v, want < %v", floodRows,
			best, floodScanBudget)
	}
	t.Logf("schema guard scan (%d invariants) over %d legacy rows: best of 3 %v",
		len(ledgerRows.invariants), floodRows, best)
	// The newest hashes are synthetic and the recorder writes nothing, so
	// every scan records every identity (idle members included) again.
	if recorder.identities() != len(ledgerRows.lastHash) {
		t.Fatalf("scans recorded %d identities, want each of %d once", recorder.identities(),
			len(ledgerRows.lastHash))
	}
}

type floodDetector []schemaguard.Invariant

func (d floodDetector) Detect(context.Context) ([]schemaguard.Invariant, error) {
	return d, nil
}

type floodRouter struct{}

func (floodRouter) Route(context.Context, schemaguard.Remediation) error { return nil }

// floodRecorder counts the identities recorded (in memory: the ledger is
// left as seeded, so every scan reads the same history).
type floodRecorder struct{ seen map[string]int }

func (r *floodRecorder) Record(_ context.Context, record schemaguard.DecisionRecord) error {
	if r.seen == nil {
		r.seen = map[string]int{}
	}
	r.seen[record.Identity]++
	return nil
}

func (r *floodRecorder) identities() int { return len(r.seen) }

// bestOf runs fn n times and returns its fastest run.
func bestOf(t *testing.T, n int, fn func() error) time.Duration {
	t.Helper()
	best := time.Duration(1<<63 - 1)
	for range n {
		start := time.Now()
		if err := fn(); err != nil {
			t.Fatalf("timed run: %v", err)
		}
		best = min(best, time.Since(start))
	}
	return best
}
