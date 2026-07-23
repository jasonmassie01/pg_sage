package autonomy

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/custodian/freeze"
	"github.com/pg-sage/sidecar/internal/custodian/wal"
	"github.com/pg-sage/sidecar/internal/schema"
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
	dsn := strings.TrimSpace(os.Getenv("SAGE_TEST_DATABASE_URL"))
	if dsn == "" {
		t.Skip("SAGE_TEST_DATABASE_URL is required for PostgreSQL adapter tests")
	}
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

func TestPostgresFreezeCustodianScansLiveCatalog(t *testing.T) {
	pool := requireAutonomyDB(t)
	ctx := context.Background()
	const table = "autonomy_freeze_probe"
	if _, err := pool.Exec(ctx, "CREATE TABLE IF NOT EXISTS "+table+" (id bigint)"); err != nil {
		t.Fatalf("create probe table: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+table) })
	custodian := NewPostgresFreezeCustodian(pool, "testdb", 99)
	proposals, err := custodian.Scan(ctx)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	for _, proposal := range proposals {
		if len(proposal.TargetObjects) != 1 ||
			strings.Contains(strings.ToUpper(proposal.SQL+proposal.Plan), "VACUUM FULL") {
			t.Fatalf("unsafe or unscoped freeze proposal: %#v", proposal)
		}
	}
	rate, err := custodian.transactionRate(ctx)
	if err != nil || rate <= 0 {
		t.Fatalf("transactionRate = %v, %v", rate, err)
	}
}

func TestFreezeAdapterRowsAndThresholdDefaults(t *testing.T) {
	custodian := NewPostgresFreezeCustodian(nil, "orders", -1)
	red, amber := freezeThresholds(-1)
	if red != 25 || amber != 50 || custodian.threshold.RedBufferPct != red {
		t.Fatalf("thresholds = %v/%v custodian=%#v", red, amber, custodian.threshold)
	}
	proposal, err := custodian.scanRow(adapterRow{values: []any{
		"public", "orders", int64(90), int64(100), int64(1), int64(100),
	}}, 1)
	if err != nil || proposal.SQL == "" || proposal.Deadline == nil {
		t.Fatalf("scanRow proposal=%#v err=%v", proposal, err)
	}
	if proposal.Deadline.HardAt.Before(time.Now()) {
		t.Fatalf("deadline already expired: %#v", proposal.Deadline)
	}
	_, err = custodian.scanRow(adapterRow{values: []any{
		"", "orders", int64(1), int64(100), int64(1), int64(100),
	}}, 1)
	if err == nil {
		t.Fatal("invalid freeze identity was accepted")
	}
}

func TestFreezeAdapterBuildsBlockerTuneAndBloatResponses(t *testing.T) {
	custodian := NewPostgresFreezeCustodian(nil, "orders", 25)
	blocker := &freeze.XminBlocker{PID: 42, XminAge: 900, User: "app"}
	red, err := custodian.scanResponseRow(adapterRow{values: []any{
		"public", "orders", int64(90), int64(100), int64(1), int64(100), 0.1,
	}}, 1, blocker, false)
	if err != nil || red.Feature != "freeze_blocker" || red.Evidence["pid"] != 42 {
		t.Fatalf("blocker proposal=%#v err=%v", red, err)
	}
	amber, err := custodian.scanResponseRow(adapterRow{values: []any{
		"public", "orders", int64(60), int64(100), int64(1), int64(100), 0.25,
	}}, 1, nil, false)
	if err != nil || amber.Feature != "autovacuum_tuning" || amber.SQL == "" {
		t.Fatalf("autovacuum proposal=%#v err=%v", amber, err)
	}
	bloat, err := custodian.scanResponseRow(adapterRow{values: []any{
		"public", "orders", int64(1), int64(100), int64(1), int64(100), 0.6,
	}}, 1, nil, true)
	if err != nil || bloat.Plan == "" || bloat.SQL != "" {
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
		"orders_slot", "logical", true, int64(defaultWALBackstopBytes + 1),
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
		custodian.options.RetainedBytesLimit != defaultWALBackstopBytes {
		t.Fatalf("WAL defaults = %#v", custodian.options)
	}
	bytes, known := custodian.diskCapacity(context.Background())
	if bytes != 1000 || !known {
		t.Fatalf("disk capacity = %d/%v", bytes, known)
	}
	custodian.options.Disk = fixedDisk{err: errors.New("unavailable")}
	if _, known := custodian.diskCapacity(context.Background()); known {
		t.Fatal("failed disk provider was treated as known")
	}
	policy := custodian.policy()
	if policy.AllowDrop || policy.RetainedWALBytesThreshold != defaultWALBackstopBytes {
		t.Fatalf("WAL policy = %#v", policy)
	}
	const escapedDropSQL = "SELECT pg_drop_replication_slot('owner''s')"
	if got := custodian.dropProposal("owner's").SQL; got != escapedDropSQL {
		t.Fatalf("escaped drop SQL = %q", got)
	}
	if got := custodian.boundProposal("slot"); got.Feature != "wal" || got.SQL == "" {
		t.Fatalf("bound proposal = %#v", got)
	}
	decision, err := wal.Classify(context.Background(), wal.SlotEvidence{
		Active: true, RetainedWALKnown: true,
		RetainedWALBytes: defaultWALBackstopBytes + 1,
	}, policy)
	if err != nil || decision.Action != wal.ActionBound {
		t.Fatalf("degraded evidence decision=%#v err=%v", decision, err)
	}
}

func TestWALBackstopSuppressesRepeatedOrStricterSetting(t *testing.T) {
	limit := defaultWALBackstopBytes
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
