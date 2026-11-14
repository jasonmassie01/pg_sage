package autonomy

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/custodian/freeze"
	"github.com/pg-sage/sidecar/internal/custodian/wal"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

var (
	autonomySchemaOnce sync.Once
	autonomySchemaErr  error
)

type adapterRow struct{ values []any }

func (r adapterRow) Scan(dest ...any) error {
	for index, value := range r.values {
		switch target := dest[index].(type) {
		case *string:
			*target = value.(string)
		case *bool:
			*target = value.(bool)
		case *int64:
			*target = value.(int64)
		case *float64:
			*target = value.(float64)
		case **float64:
			*target = value.(*float64)
		}
	}
	return nil
}

type fixedDisk struct {
	bytes int64
	err   error
}

func (d fixedDisk) CapacityBytes(context.Context) (int64, error) {
	return d.bytes, d.err
}

func requireAutonomyDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect PostgreSQL: %v", err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		pool.Close()
		t.Fatalf("ping PostgreSQL: %v", err)
	}
	autonomySchemaOnce.Do(func() {
		autonomySchemaErr = schema.Bootstrap(context.Background(), pool)
	})
	if autonomySchemaErr != nil {
		pool.Close()
		t.Fatalf("bootstrap autonomy schema: %v", autonomySchemaErr)
	}
	t.Cleanup(pool.Close)
	return pool
}

// exclusiveAutonomyDB is requireAutonomyDB on a fresh database of the
// exclusive test server, held for t alone (testdb.Exclusive): for tests
// that measure XID ages, which every other package's writes move (ci-13).
func exclusiveAutonomyDB(t *testing.T, reason string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), testdb.Exclusive(t, reason))
	if err != nil {
		t.Fatalf("connect the exclusive test database: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(context.Background(), pool); err != nil {
		t.Fatalf("bootstrap the exclusive test database: %v", err)
	}
	return pool
}

// vacuum-09: an empty, just-frozen table is green, and the scan proposes
// nothing for it (counted, not looped over: before, this test passed with
// any number of proposals, zero included). The sampler's second read
// measures both consumption rates.
func TestPostgresFreezeCustodianScansLiveCatalog(t *testing.T) {
	pool := requireAutonomyDB(t)
	ctx := context.Background()
	const table = "autonomy_freeze_probe"
	for _, sql := range []string{"DROP TABLE IF EXISTS " + table,
		"CREATE TABLE " + table + " (id bigint)", "VACUUM (FREEZE) " + table} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+table) })
	custodian := NewPostgresFreezeCustodian(pool, "testdb", 25)
	for scan := 1; scan <= 2; scan++ {
		proposals, err := custodian.Scan(ctx)
		if err != nil {
			t.Fatalf("Scan %d: %v", scan, err)
		}
		if n := len(proposalsFor(proposals, "public."+table)); n != 0 {
			t.Fatalf("scan %d: %d proposals for the empty green table, want 0: %#v",
				scan, n, proposals)
		}
		for _, proposal := range proposals {
			if len(proposal.TargetObjects) != 1 ||
				strings.Contains(strings.ToUpper(proposal.SQL+proposal.Plan), "VACUUM FULL") {
				t.Fatalf("scan %d: unsafe or unscoped freeze proposal: %#v", scan, proposal)
			}
		}
	}
	reading, err := custodian.Sampler().Read(ctx)
	rates := reading.rates
	if err != nil || !rates.known || rates.xid <= 0 || rates.mxid <= 0 {
		t.Fatalf("transactionRates = %#v, %v", rates, err)
	}
	if len(reading.Horizons) == 0 {
		t.Fatal("the sampler read no horizons from a live catalog")
	}
}

func TestFreezeAdapterRowsAndThresholdDefaults(t *testing.T) {
	custodian := NewPostgresFreezeCustodian(nil, "orders", -1)
	defaults := freeze.BufferThresholds(-1)
	red, amber := defaults.RedBufferPct, defaults.AmberBufferPct
	if red != 25 || amber != 50 || custodian.threshold.RedBufferPct != red {
		t.Fatalf("thresholds = %v/%v custodian=%#v", red, amber, custodian.threshold)
	}
	proposal, err := custodian.scanResponseRow(adapterRow{values: []any{
		"public", "orders", int64(90), int64(100), int64(1), int64(100), 0.0,
	}}, knownRates, nil, false)
	if err != nil || proposal.SQL == "" || proposal.Deadline == nil {
		t.Fatalf("scanResponseRow proposal=%#v err=%v", proposal, err)
	}
	if proposal.Deadline.HardAt.Before(time.Now()) {
		t.Fatalf("deadline already expired: %#v", proposal.Deadline)
	}
	_, err = custodian.scanResponseRow(adapterRow{values: []any{
		"", "orders", int64(1), int64(100), int64(1), int64(100), 0.0,
	}}, knownRates, nil, false)
	if err == nil {
		t.Fatal("invalid freeze identity was accepted")
	}
}

var knownRates = freezeRateSample{xid: 1, mxid: 1, known: true}

func TestFreezeAdapterBuildsBlockerAndBloatResponses(t *testing.T) {
	custodian := NewPostgresFreezeCustodian(nil, "orders", 25)
	// vacuum-02: a red table at 160M with a 30-XID session is frozen; the
	// session cannot pin it (before, the session was canceled instead).
	holders := []freeze.XminBlocker{{Kind: freeze.HolderSession, PID: 42, XminAge: 30,
		User: "app", State: "active"}}
	red, err := custodian.scanResponseRow(adapterRow{values: []any{
		"public", "orders", int64(160_000_000), int64(200_000_000), int64(1), int64(100), 0.1,
	}}, knownRates, holders, false)
	if err != nil || red.Feature != "freeze" ||
		red.SQL != `VACUUM (FREEZE) "public"."orders"` || red.Evidence["xmin_holder"] != nil {
		t.Fatalf("red proposal with a young holder=%#v err=%v", red, err)
	}
	amber, err := custodian.scanResponseRow(adapterRow{values: []any{
		"public", "orders", int64(60), int64(100), int64(1), int64(100), 0.25,
	}}, knownRates, nil, false)
	// vacuum-07: the custodian no longer tunes autovacuum; an amber table
	// with a 0.25 dead-tuple ratio gets nothing (the analyzer owns tuning).
	if err != nil || amber.SQL != "" || amber.Plan != "" || amber.ResponseKind != "" {
		t.Fatalf("amber custodian proposal=%#v err=%v, want none", amber, err)
	}
	// vacuum-04: a 0.6 dead-tuple ratio is not bloat (the analyzer's
	// VACUUM owns it); only the catalog estimate on a table at the size
	// floor plans a rebuild, typed by its response kind.
	dead, err := custodian.scanResponseRow(adapterRow{values: []any{
		"public", "orders", int64(1), int64(100), int64(1), int64(100), 0.6,
	}}, knownRates, nil, true)
	if err != nil || dead.Plan != "" || dead.SQL != "" || dead.ResponseKind != "" {
		t.Fatalf("dead-tuple ratio became a bloat plan=%#v err=%v", dead, err)
	}
	pages := int64(1)<<30/8192 + 1
	bloat, err := custodian.scanResponseRow(horizonValues("orders", 0.6, 7, pages,
		pagesPtr(float64(pages)/4)), knownRates, nil, true)
	if err != nil || bloat.Plan == "" || bloat.SQL != "" ||
		bloat.ResponseKind != string(freeze.ResponsePlanRepack) ||
		bloat.Feature != "bloat_remediation" || bloat.Evidence["plan"] != bloat.Plan {
		t.Fatalf("bloat proposal=%#v err=%v", bloat, err)
	}
	if strings.Contains(strings.ToUpper(bloat.Plan), "VACUUM FULL") {
		t.Fatalf("unsafe bloat plan=%q", bloat.Plan)
	}
}

func TestPostgresWALCustodianUsesByteBackstopWithoutDiskEvidence(t *testing.T) {
	pool := requireAutonomyDB(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS sage;
		CREATE TABLE IF NOT EXISTS sage.slot_consumer_registry (
			slot_name text PRIMARY KEY, owner_tag text, registered boolean NOT NULL
		)`); err != nil {
		t.Fatalf("create slot registry: %v", err)
	}
	custodian := NewPostgresWALCustodian(pool, "testdb", PostgresWALOptions{})
	proposal, err := custodian.scanSlot(ctx, adapterRow{values: []any{
		"orders_slot", "logical", true, int64(DefaultWALBackstopBytes + 1),
	}}, 0, false)
	if err != nil {
		t.Fatalf("scanSlot: %v", err)
	}
	if !strings.Contains(proposal.SQL, "max_slot_wal_keep_size") {
		t.Fatalf("backstop proposal = %#v", proposal)
	}
	if _, err := custodian.Scan(ctx); err != nil {
		t.Fatalf("live slot Scan: %v", err)
	}
}

func TestWALAdapterDefaultsEvidenceAndEscaping(t *testing.T) {
	custodian := NewPostgresWALCustodian(nil, "orders", PostgresWALOptions{
		Disk: fixedDisk{bytes: 1000},
	})
	if custodian.options.AbandonAfter != 24*time.Hour ||
		custodian.options.RetainedBytesLimit != DefaultWALBackstopBytes {
		t.Fatalf("WAL defaults = %#v", custodian.options)
	}
	capacity := custodian.diskCapacity(context.Background())
	if capacity.bytes != 1000 || !capacity.known {
		t.Fatalf("disk capacity = %d/%v", capacity.bytes, capacity.known)
	}
	custodian.options.Disk = fixedDisk{err: errors.New("unavailable")}
	if custodian.diskCapacity(context.Background()).known {
		t.Fatal("failed disk provider was treated as known")
	}
	policy := custodian.policy()
	if policy.AllowDrop || policy.RetainedWALBytesThreshold != DefaultWALBackstopBytes {
		t.Fatalf("WAL policy = %#v", policy)
	}
	const escapedDropSQL = "SELECT pg_drop_replication_slot('owner''s')"
	if got := custodian.dropProposal("owner's").SQL; got != escapedDropSQL {
		t.Fatalf("escaped drop SQL = %q", got)
	}
	got := custodian.boundProposal("slot", DefaultWALBackstopBytes)
	if got.Feature != "wal" || got.SQL == "" {
		t.Fatalf("bound proposal = %#v", got)
	}
	decision, err := wal.Classify(context.Background(), wal.SlotEvidence{
		Active: true, RetainedWALKnown: true,
		RetainedWALBytes: DefaultWALBackstopBytes + 1,
	}, policy)
	if err != nil || decision.Action != wal.ActionBound {
		t.Fatalf("degraded evidence decision=%#v err=%v", decision, err)
	}
}

func TestWALBackstopSuppressesRepeatedOrStricterSetting(t *testing.T) {
	limit := DefaultWALBackstopBytes
	for _, current := range []int64{limit, limit / 2} {
		if needsWALBackstop(current, limit) {
			t.Fatalf("current setting %d should suppress %d-byte backstop", current, limit)
		}
	}
	if !needsWALBackstop(limit*2, limit) {
		t.Fatal("looser current setting did not require backstop")
	}
}

func TestFreezePolicyDeadlineDoesNotEscalateAmber(t *testing.T) {
	if deadline := freezePolicyDeadline(freeze.Proposal{
		Urgency: freeze.UrgencyAmber,
	}); deadline != nil {
		t.Fatalf("amber deadline = %#v", deadline)
	}
}
