package autonomy

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
	"github.com/pg-sage/sidecar/internal/ledger"
	"github.com/pg-sage/sidecar/internal/schemaguard"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// maxReadsPerCycle bounds the schema guard's own queries in one cycle,
// independent of how many invariants the catalog holds: three detection
// queries, the statement index, the schema shapes, the session check,
// one contract and one history lookup, with headroom.
const maxReadsPerCycle = 12

// queryCounter is a pgx tracer that records every statement a pool sends.
type queryCounter struct {
	mu  sync.Mutex
	sql []string
}

func (c *queryCounter) TraceQueryStart(
	ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData,
) context.Context {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sql = append(c.sql, data.SQL)
	return ctx
}

func (*queryCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (c *queryCounter) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sql = nil
}

// counts returns the reads and the sage.decision inserts sent since reset.
func (c *queryCounter) counts() (reads, decisionWrites int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, sql := range c.sql {
		if strings.Contains(sql, "INSERT INTO sage.decision") {
			decisionWrites++
			continue
		}
		reads++
	}
	return reads, decisionWrites
}

func tracedPool(t *testing.T) (*pgxpool.Pool, *queryCounter) {
	t.Helper()
	requireAutonomyDB(t)
	config, err := pgxpool.ParseConfig(testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatalf("parse test DSN: %v", err)
	}
	counter := &queryCounter{}
	config.ConnConfig.Tracer = counter
	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatalf("connect traced pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool, counter
}

func countGuardRows(t *testing.T, pool *pgxpool.Pool, targets []string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM sage.decision
		WHERE feature='schema_guard' AND target_objects ?| $1::text[]`, targets).
		Scan(&n); err != nil {
		t.Fatalf("count schema guard rows: %v", err)
	}
	return n
}

func routesTo(router *recordingRouter, targets []string) int {
	wanted := make(map[string]bool, len(targets))
	for _, target := range targets {
		wanted[target] = true
	}
	n := 0
	for _, proposal := range router.routed() {
		for _, object := range proposal.TargetObjects {
			if wanted[object] {
				n++
			}
		}
	}
	return n
}

func scanGuard(t *testing.T, guard *schemaguard.Custodian) schemaguard.CycleResult {
	t.Helper()
	result, err := guard.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	return result
}

// A synthetic catalog the size of the dogfood problem (2,000 tables in 40
// identical tenant schemas, plus 40 idle leftover copies): decision rows
// per cycle are O(changes), never O(invariants), and the guard's own reads
// per cycle are bounded.
func TestSchemaGuardLoadIsBoundedOnASyntheticCatalog(t *testing.T) {
	if testing.Short() {
		t.Skip("synthetic 2,400-table catalog skipped in -short mode")
	}
	base := requireAutonomyDB(t)
	live := createCloneFamily(t, base, "sgllive", 40, 49)
	idle := createTextFamily(t, base, "sglidle", 40, 9)
	defer holdFamilyLock(t, base, live, live.parent())()
	pool, counter := tracedPool(t)
	router := &recordingRouter{}
	guard, err := NewPostgresSchemaGuard(pool, "testdb", router,
		ledger.NewService(ledger.NewPostgresRepository(pool)), allowRetention,
		SchemaGuardOptions{})
	if err != nil {
		t.Fatalf("NewPostgresSchemaGuard: %v", err)
	}
	liveTargets, idleTargets := live.childTargets(), idle.childTargets()
	first := requireFirstScan(t, base, guard, router, live, idle)
	counter.reset()
	second := scanGuard(t, guard)
	reads, writes := counter.counts()
	if reads > maxReadsPerCycle || second.Detected < 40*49+40*9 {
		t.Fatalf("second scan: %d reads for %d invariants, want <= %d", reads,
			second.Detected, maxReadsPerCycle)
	}
	if writes != second.Recorded || second.Unchanged < 49+9 ||
		countGuardRows(t, base, append(liveTargets, idleTargets...)) != 58 {
		t.Fatalf("unchanged second scan wrote rows: %+v writes=%d", second, writes)
	}
	if got := routesTo(router, liveTargets); got != 2*40*49 {
		t.Fatalf("live routes after two scans = %d, want 3920 (routing unchanged)", got)
	}
	if first.Detected != second.Detected {
		t.Fatalf("detected %d then %d on an unchanged catalog", first.Detected, second.Detected)
	}
	requireOneChangeRecorded(t, base, guard, live)
}

// requireFirstScan checks the first cycle: one row per identity, every
// live member routed, the idle leftover parked and skipped.
func requireFirstScan(
	t *testing.T, base *pgxpool.Pool, guard *schemaguard.Custodian,
	router *recordingRouter, live, idle cloneFamily,
) schemaguard.CycleResult {
	t.Helper()
	liveTargets, idleTargets := live.childTargets(), idle.childTargets()
	first := scanGuard(t, guard)
	if rows := countGuardRows(t, base, liveTargets); rows != 49 {
		t.Fatalf("live family rows after first scan = %d, want 49 (one per foreign key)", rows)
	}
	if rows := countGuardRows(t, base, idleTargets); rows != 9 {
		t.Fatalf("idle family rows after first scan = %d, want 9", rows)
	}
	if got := routesTo(router, liveTargets); got != 40*49 {
		t.Fatalf("live routes = %d, want every member routed (1960)", got)
	}
	if got := routesTo(router, idleTargets); got != 0 || first.Skipped != 40*9 ||
		guardVerdict(t, base, idleTargets[0]) != string(ledger.VerdictPark) {
		t.Fatalf("idle leftover family: routes=%d skipped=%d verdict=%q, want 0 routes, "+
			"360 skipped and a park (family: %s)", got, first.Skipped,
			guardVerdict(t, base, idleTargets[0]), familyReason(t, base, idleTargets[0]))
	}
	requireFannedOutRow(t, base, live)
	return first
}

// familyReason is the family classification recorded for target.
func familyReason(t *testing.T, pool *pgxpool.Pool, target string) string {
	t.Helper()
	var reason *string
	err := pool.QueryRow(context.Background(), `SELECT evidence->>'family_reason'
		FROM sage.decision WHERE feature='schema_guard' AND target_objects ? $1
		ORDER BY id DESC LIMIT 1`, target).Scan(&reason)
	if err != nil || reason == nil {
		return fmt.Sprintf("none recorded (%v)", err)
	}
	return *reason
}

// guardVerdict is the latest schema guard verdict recorded for target.
func guardVerdict(t *testing.T, pool *pgxpool.Pool, target string) string {
	t.Helper()
	var verdict string
	if err := pool.QueryRow(context.Background(), `SELECT verdict FROM sage.decision
		WHERE feature='schema_guard' AND target_objects ? $1 ORDER BY id DESC LIMIT 1`,
		target).Scan(&verdict); err != nil {
		t.Fatalf("read verdict for %s: %v", target, err)
	}
	return verdict
}

// requireFannedOutRow checks a live-family row lists all 40 members.
func requireFannedOutRow(t *testing.T, pool *pgxpool.Pool, live cloneFamily) {
	t.Helper()
	var targets []byte
	var state string
	var count int
	err := pool.QueryRow(context.Background(), `SELECT target_objects,
		evidence->>'family_state', (evidence->>'affected_schema_count')::int
		FROM sage.decision WHERE feature='schema_guard' AND target_objects ? $1
		ORDER BY id DESC LIMIT 1`, live.schemas[0]+"."+live.child(1)).
		Scan(&targets, &state, &count)
	if err != nil {
		t.Fatalf("read live family row: %v", err)
	}
	var list []string
	if err := json.Unmarshal(targets, &list); err != nil {
		t.Fatalf("decode targets: %v", err)
	}
	if len(list) != 40 || state != "live" || count != 40 {
		t.Fatalf("live row targets=%d state=%q count=%d, want 40 live members",
			len(list), state, count)
	}
}

// requireOneChangeRecorded fixes one foreign key in one member: the next
// cycle records exactly the one changed identity.
func requireOneChangeRecorded(
	t *testing.T, pool *pgxpool.Pool, guard *schemaguard.Custodian, live cloneFamily,
) {
	t.Helper()
	targets := live.childTargets()
	before := countGuardRows(t, pool, targets)
	execAll(t, pool, "CREATE INDEX ON "+live.schemas[3]+"."+live.child(7)+" (p_id)")
	result := scanGuard(t, guard)
	if after := countGuardRows(t, pool, targets); after != before+1 {
		t.Fatalf("rows after one fixed foreign key = %d, want %d (+1); result=%+v",
			after, before+1, result)
	}
	var members int
	if err := pool.QueryRow(context.Background(), `SELECT jsonb_array_length(target_objects)
		FROM sage.decision WHERE feature='schema_guard' AND target_objects ? $1
		ORDER BY id DESC LIMIT 1`, live.schemas[0]+"."+live.child(7)).Scan(&members); err != nil {
		t.Fatalf("read changed row: %v", err)
	}
	if members != 39 {
		t.Fatalf("changed identity covers %d schemas, want 39", members)
	}
}

// countingGuard counts scans of a real schema guard.
type countingGuard struct {
	inner SchemaGuard
	mu    sync.Mutex
	calls int
}

func (g *countingGuard) Scan(ctx context.Context) (schemaguard.CycleResult, error) {
	g.mu.Lock()
	g.calls++
	g.mu.Unlock()
	return g.inner.Scan(ctx)
}

func (g *countingGuard) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

// A DDL burst (an app test suite creating tables, each followed by the
// post-DDL request) drives at most one extra scan per debounce window.
func TestPostDDLBurstIsCoalescedAgainstPostgres(t *testing.T) {
	pool := requireAutonomyDB(t)
	inner, err := NewPostgresSchemaGuard(pool, "testdb", &recordingRouter{},
		ledger.NewService(ledger.NewPostgresRepository(pool)), allowRetention,
		SchemaGuardOptions{})
	if err != nil {
		t.Fatalf("NewPostgresSchemaGuard: %v", err)
	}
	guard := &countingGuard{inner: inner}
	supervisor := debounceSupervisor(t, guard, 2*time.Second, make(chan time.Time))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	supervisor.Start(ctx)
	requestN(t, supervisor, 1)
	waitFor(t, 10*time.Second, func() bool { return guard.count() == 1 })
	burst := createCloneFamily(t, pool, "sglburst", 1, 0)
	for i := 0; i < 30; i++ {
		execAll(t, pool, "CREATE TABLE "+burst.schemas[0]+"."+burst.child(i+1)+" (id int)")
		requestN(t, supervisor, 1)
	}
	if got := guard.count(); got > 2 {
		t.Fatalf("scans during a 30-DDL burst = %d, want at most 2", got)
	}
	waitFor(t, 10*time.Second, func() bool { return guard.count() == 2 })
	time.Sleep(2500 * time.Millisecond)
	if got := guard.count(); got != 2 {
		t.Fatalf("scans after the burst = %d, want exactly 2", got)
	}
	shutdownSupervisor(t, supervisor)
}

// A changed decision on an ordinary table is recorded and routing follows
// it; an unchanged one is not re-recorded.
func TestSchemaGuardRecordsChangesAndRoutingFollows(t *testing.T) {
	pool := requireAutonomyDB(t)
	family := createCloneFamily(t, pool, "sglchg", 1, 1)
	target := family.schemas[0] + "." + family.child(1)
	router := &recordingRouter{}
	guard, err := NewPostgresSchemaGuard(pool, "testdb", router,
		ledger.NewService(ledger.NewPostgresRepository(pool)), allowRetention,
		SchemaGuardOptions{})
	if err != nil {
		t.Fatalf("NewPostgresSchemaGuard: %v", err)
	}
	scanGuard(t, guard)
	scanGuard(t, guard)
	if rows, routes := countGuardRows(t, pool, []string{target}),
		routesTo(router, []string{target}); rows != 1 || routes != 2 {
		t.Fatalf("two unchanged scans: rows=%d routes=%d, want 1 row and 2 routes",
			rows, routes)
	}
	execAll(t, pool, `INSERT INTO sage.table_contract (schema_name, table_name,
		exemptions, declared_by, evidence_id) VALUES ('`+family.schemas[0]+`', '`+
		family.child(1)+`', '["missing_fk_index"]'::jsonb, 'test', 'sglchg_`+family.tag+`')`)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.table_contract WHERE evidence_id=$1", "sglchg_"+family.tag)
	})
	scanGuard(t, guard)
	var verdict, reason string
	if err := pool.QueryRow(context.Background(), `SELECT verdict, reason FROM sage.decision
		WHERE feature='schema_guard' AND target_objects ? $1 ORDER BY id DESC LIMIT 1`,
		target).Scan(&verdict, &reason); err != nil {
		t.Fatalf("read changed decision: %v", err)
	}
	if verdict != string(ledger.VerdictPark) || reason != "table contract exemption" ||
		routesTo(router, []string{target}) != 2 ||
		countGuardRows(t, pool, []string{target}) != 2 {
		t.Fatalf("exempted: verdict=%q reason=%q routes=%d, want a new park and no route",
			verdict, reason, routesTo(router, []string{target}))
	}
}
