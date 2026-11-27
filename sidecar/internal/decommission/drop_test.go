package decommission

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

const dropActor = "admin@example.test"

// estateDB is a bootstrapped control database carrying the legacy tables and
// the eight-item estate.
func estateDB(t *testing.T, label string) (*pgxpool.Pool, context.Context) {
	t.Helper()
	pool, ctx := freshDB(t, label)
	bootstrapped(t, ctx, pool)
	applyLegacySchema(t, ctx, pool)
	seedEstate(t, ctx, pool)
	return pool, ctx
}

func mustBuild(t *testing.T, ctx context.Context, pool *pgxpool.Pool) Inventory {
	t.Helper()
	inv, err := Build(ctx, pool)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return inv
}

// ackAllBut acknowledges every inventory item except the ids in keep.
func ackAllBut(t *testing.T, ctx context.Context, pool *pgxpool.Pool, keep ...string) {
	t.Helper()
	skip := map[string]bool{}
	for _, id := range keep {
		skip[id] = true
	}
	var ids []string
	for _, it := range mustBuild(t, ctx, pool).Items {
		if !skip[it.ID] {
			ids = append(ids, it.ID)
		}
	}
	if len(ids) == 0 {
		return
	}
	if _, err := Acknowledge(ctx, pool, AckRequest{AcknowledgedResources: ids,
		Exported: true}, dropActor, SourceAPI); err != nil {
		t.Fatalf("acknowledge: %v", err)
	}
}

func dropReq(inv Inventory) DropRequest {
	return DropRequest{InventorySHA256: inv.InventorySHA256, Confirm: DropConfirmPhrase}
}

func scalarInt(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string,
	args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	return n
}

// legacyCount is CHECK-01's query.
func legacyCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int {
	t.Helper()
	return scalarInt(t, ctx, pool, `SELECT count(*)::int FROM pg_tables
		WHERE schemaname = 'sage' AND tablename = ANY($1)`, LegacyTables)
}

// dropAuditCount is CHECK-02's query.
func dropAuditCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int {
	t.Helper()
	return scalarInt(t, ctx, pool, `SELECT count(*)::int FROM sage.config_audit
		WHERE key LIKE 'decommission.legacy_table.%'`)
}

func TestDropConfirmPhrase_NamesTheTableCount(t *testing.T) {
	if DropConfirmPhrase != "drop 27 legacy tables" || len(LegacyTables) != 27 {
		t.Fatalf("phrase %q for %d tables", DropConfirmPhrase, len(LegacyTables))
	}
}

func TestValidateDrop_RefusesBadInput(t *testing.T) {
	good := strings.Repeat("ab", 32)
	cases := []struct {
		name  string
		req   DropRequest
		actor string
		want  error
	}{
		{"wrong phrase", DropRequest{good, "drop the tables"}, dropActor, ErrBadConfirm},
		{"empty phrase", DropRequest{good, ""}, dropActor, ErrBadConfirm},
		{"padded phrase", DropRequest{good, DropConfirmPhrase + " "}, dropActor,
			ErrBadConfirm},
		{"upper phrase", DropRequest{good, strings.ToUpper(DropConfirmPhrase)}, dropActor,
			ErrBadConfirm},
		{"no digest", DropRequest{"", DropConfirmPhrase}, dropActor, ErrBadDigest},
		{"short digest", DropRequest{good[:63], DropConfirmPhrase}, dropActor, ErrBadDigest},
		{"long digest", DropRequest{good + "a", DropConfirmPhrase}, dropActor, ErrBadDigest},
		{"not hex", DropRequest{strings.Repeat("zz", 32), DropConfirmPhrase}, dropActor,
			ErrBadDigest},
		{"upper hex", DropRequest{strings.Repeat("AB", 32), DropConfirmPhrase}, dropActor,
			ErrBadDigest},
		{"no actor", DropRequest{good, DropConfirmPhrase}, "  ", ErrNoActor},
	}
	for _, c := range cases {
		if err := validateDrop(c.req, c.actor); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.want)
		}
	}
	if err := validateDrop(DropRequest{good, DropConfirmPhrase}, dropActor); err != nil {
		t.Fatalf("a well-formed request was refused: %v", err)
	}
}

func TestDropLegacyTables_NilPool(t *testing.T) {
	_, err := DropLegacyTables(context.Background(), nil,
		DropRequest{strings.Repeat("ab", 32), DropConfirmPhrase}, dropActor)
	if err == nil || !strings.Contains(err.Error(), "no control database pool") {
		t.Fatalf("nil pool err = %v", err)
	}
}

// State transition: one unacknowledged item keeps all 27 tables.
func TestDrop_RefusesWhileAnItemIsUnacknowledged(t *testing.T) {
	pool, ctx := estateDB(t, "decom_drop_unack")
	ackAllBut(t, ctx, pool, "provider_resource:d-live")
	inv := mustBuild(t, ctx, pool)
	if inv.Unacknowledged != 1 {
		t.Fatalf("setup: %d unacknowledged, want 1", inv.Unacknowledged)
	}
	res, err := DropLegacyTables(ctx, pool, dropReq(inv), dropActor)
	if !errors.Is(err, ErrUnacknowledged) || !strings.Contains(err.Error(), "1 ") {
		t.Fatalf("err = %v, want ErrUnacknowledged naming 1 item", err)
	}
	if len(res.Dropped) != 0 || legacyCount(t, ctx, pool) != 27 ||
		dropAuditCount(t, ctx, pool) != 0 {
		t.Fatalf("a refused drop changed state: res %v, %d tables, %d audit rows",
			res.Dropped, legacyCount(t, ctx, pool), dropAuditCount(t, ctx, pool))
	}
}

// State transition: export, acknowledge everything, drop. The digest is the
// one exported before acknowledging, so acknowledging never makes it stale.
func TestDrop_AfterEveryAckDropsExactlyTheLegacyTables(t *testing.T) {
	clearProviderEnv(t)
	pool, ctx := estateDB(t, "decom_drop_all")
	mustExec(t, ctx, pool, `INSERT INTO sage.agent_db_agent_tokens (token_id, tenant_id,
		agent_id, token_hash, expires_at) VALUES ('tk1', 't1', 'a1', 'h1',
		now() + interval '1 day')`)
	if n, err := CountLiveLegacyTokens(ctx, pool); err != nil || n != 1 {
		t.Fatalf("setup: live legacy tokens = %d, %v", n, err)
	}
	exported := mustBuild(t, ctx, pool)
	ackAllBut(t, ctx, pool)
	res, err := DropLegacyTables(ctx, pool, dropReq(exported), dropActor)
	if err != nil {
		t.Fatalf("drop: %v", err)
	}
	if !reflect.DeepEqual(res.Dropped, LegacyTables) {
		t.Fatalf("dropped %v, want the 27 legacy tables in order", res.Dropped)
	}
	if n := legacyCount(t, ctx, pool); n != 0 {
		t.Fatalf("CHECK-01: %d legacy tables remain", n)
	}
	assertDropAudit(t, ctx, pool, LegacyTables)
	assertCleanAfterDrop(t, ctx, pool)
}

func assertDropAudit(t *testing.T, ctx context.Context, pool *pgxpool.Pool, want []string) {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT key, old_value, new_value, changed_by_actor
		FROM sage.config_audit WHERE key LIKE 'decommission.legacy_table.%' ORDER BY id`)
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var key, oldV, newV, actor string
		if err := rows.Scan(&key, &oldV, &newV, &actor); err != nil {
			t.Fatalf("scan audit: %v", err)
		}
		if oldV != "present" || newV != "dropped" || actor != dropActor {
			t.Errorf("audit %s: %q -> %q by %q", key, oldV, newV, actor)
		}
		got = append(got, strings.TrimPrefix(key, "decommission.legacy_table."))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read audit: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("CHECK-02: audit rows for %v, want %v", got, want)
	}
}

// assertCleanAfterDrop: the inventory, startup and token warning go quiet,
// and the acknowledgement table stays.
func assertCleanAfterDrop(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	inv := mustBuild(t, ctx, pool)
	if inv.LegacyTables || len(inv.Items) != 0 || inv.Unacknowledged != 0 {
		t.Fatalf("after the drop: legacy %v, %d items", inv.LegacyTables, len(inv.Items))
	}
	var log capturedLog
	if err := Startup(ctx, pool, "", log.logger()); err != nil {
		t.Fatalf("Startup: %v", err)
	}
	if len(log.info) != 0 || len(log.warns) != 0 {
		t.Fatalf("startup logged after the drop:\n%s", log.all())
	}
	if n, err := CountLiveLegacyTokens(ctx, pool); err != nil || n != 0 {
		t.Fatalf("live legacy tokens after the drop = %d, %v", n, err)
	}
	if n := scalarInt(t, ctx, pool, `SELECT count(*)::int FROM sage.agentdb_decommission`); n != 8 {
		t.Fatalf("acknowledgements after the drop = %d, want 8 kept", n)
	}
}

// Error propagation: a user view on a legacy table fails the whole drop.
func TestDrop_DependentViewFailsTheWholeStatement(t *testing.T) {
	pool, ctx := estateDB(t, "decom_drop_view")
	ackAllBut(t, ctx, pool)
	mustExec(t, ctx, pool, `CREATE VIEW public.my_ping_report AS
		SELECT * FROM sage.agent_db_pings`)
	_, err := DropLegacyTables(ctx, pool, dropReq(mustBuild(t, ctx, pool)), dropActor)
	var dep *DependencyError
	if !errors.As(err, &dep) {
		t.Fatalf("err = %v, want a DependencyError", err)
	}
	if !strings.Contains(err.Error(), "my_ping_report") ||
		!strings.Contains(err.Error(), "agent_db_pings") {
		t.Fatalf("the error must name the view and the table: %v", err)
	}
	if n := legacyCount(t, ctx, pool); n != 27 {
		t.Fatalf("%d legacy tables remain, want all 27", n)
	}
	if n := dropAuditCount(t, ctx, pool); n != 0 {
		t.Fatalf("%d audit rows after a failed drop", n)
	}
}

// Boundary: a clean install has nothing to drop and writes no audit row.
func TestDrop_CleanInstallDropsNothing(t *testing.T) {
	pool, ctx := freshDB(t, "decom_drop_clean")
	bootstrapped(t, ctx, pool)
	res, err := DropLegacyTables(ctx, pool, dropReq(mustBuild(t, ctx, pool)), dropActor)
	if err != nil {
		t.Fatalf("drop: %v", err)
	}
	if res.Dropped == nil || len(res.Dropped) != 0 {
		t.Fatalf("dropped = %#v, want an empty, non-nil list", res.Dropped)
	}
	if n := dropAuditCount(t, ctx, pool); n != 0 {
		t.Fatalf("%d audit rows on a clean install", n)
	}
}

// Boundary: tables already gone are skipped; the rest are dropped and audited.
func TestDrop_PartialSetDropsTheRest(t *testing.T) {
	pool, ctx := freshDB(t, "decom_drop_partial")
	bootstrapped(t, ctx, pool)
	applyLegacySchema(t, ctx, pool)
	mustExec(t, ctx, pool, `DROP TABLE sage.agent_db_schema_version,
		sage.agent_db_agent_tokens`)
	var want []string
	for _, name := range LegacyTables {
		if name != "agent_db_schema_version" && name != LegacyTokensTable {
			want = append(want, name)
		}
	}
	res, err := DropLegacyTables(ctx, pool, dropReq(mustBuild(t, ctx, pool)), dropActor)
	if err != nil {
		t.Fatalf("drop: %v", err)
	}
	if !reflect.DeepEqual(res.Dropped, want) || len(want) != 25 {
		t.Fatalf("dropped %v, want the 25 remaining tables", res.Dropped)
	}
	if n := legacyCount(t, ctx, pool); n != 0 {
		t.Fatalf("%d legacy tables remain", n)
	}
	assertDropAudit(t, ctx, pool, want)
}

// Stale view: a digest exported before a new item appeared is refused, even
// once that item is acknowledged too.
func TestDrop_StaleInventoryDigestIsRefused(t *testing.T) {
	pool, ctx := estateDB(t, "decom_drop_stale")
	exported := mustBuild(t, ctx, pool)
	insertDeployment(t, ctx, pool, "d-new", "aws_rds", "active", map[string]any{
		"live_mode": true, "provider_resource_id": "pgsage-d-new"})
	ackAllBut(t, ctx, pool)
	if fresh := mustBuild(t, ctx, pool); fresh.InventorySHA256 == exported.InventorySHA256 {
		t.Fatal("a new item did not change the inventory digest")
	}
	_, err := DropLegacyTables(ctx, pool, dropReq(exported), dropActor)
	if !errors.Is(err, ErrStaleInventory) {
		t.Fatalf("err = %v, want ErrStaleInventory", err)
	}
	if n := legacyCount(t, ctx, pool); n != 27 {
		t.Fatalf("%d legacy tables remain after a stale drop, want 27", n)
	}
	if n := dropAuditCount(t, ctx, pool); n != 0 {
		t.Fatalf("%d audit rows after a stale drop", n)
	}
}

// Concurrent access: two drops at once drop the tables once.
func TestDrop_ConcurrentDropsDropOnce(t *testing.T) {
	pool, ctx := estateDB(t, "decom_drop_race")
	ackAllBut(t, ctx, pool)
	req := dropReq(mustBuild(t, ctx, pool))
	var wg sync.WaitGroup
	results := make([]DropResult, 2)
	errs := make([]error, 2)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = DropLegacyTables(ctx, pool, req, dropActor)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("drop %d: %v", i, err)
		}
	}
	a, b := len(results[0].Dropped), len(results[1].Dropped)
	if a+b != 27 || (a != 0 && b != 0) {
		t.Fatalf("drops returned %d and %d tables, want 27 and 0", a, b)
	}
	if n := dropAuditCount(t, ctx, pool); n != 27 {
		t.Fatalf("audit rows = %d, want 27", n)
	}
}

// Error propagation: without sage.config_audit the drop refuses before it
// drops anything, so no drop goes unaudited.
func TestDrop_RefusesWithoutTheAuditTable(t *testing.T) {
	pool, ctx := legacyDB(t, "decom_drop_noaudit")
	_, err := DropLegacyTables(ctx, pool, dropReq(mustBuild(t, ctx, pool)), dropActor)
	if !errors.Is(err, ErrAuditTableMissing) {
		t.Fatalf("err = %v, want ErrAuditTableMissing", err)
	}
	if n := legacyCount(t, ctx, pool); n != 27 {
		t.Fatalf("%d legacy tables remain, want 27", n)
	}
}

// Error propagation: a closed pool reports the failed step.
func TestDrop_ClosedPool(t *testing.T) {
	pool, ctx := freshDB(t, "decom_drop_closed")
	pool.Close()
	_, err := DropLegacyTables(ctx, pool,
		DropRequest{strings.Repeat("ab", 32), DropConfirmPhrase}, dropActor)
	if err == nil || !strings.Contains(err.Error(), "drop the legacy tables") {
		t.Fatalf("closed pool err = %v", err)
	}
}
