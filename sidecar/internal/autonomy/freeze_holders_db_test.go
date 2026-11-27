package autonomy

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/custodian/freeze"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// vacuum-02 on real PostgreSQL: every kind of xmin-horizon holder is
// found (a writer's backend_xid, a prepared transaction, a slot's
// catalog_xmin), named once per scan, and attached only to the red tables
// it pins.

// holderRedBuffer makes a table red from an XID age of 1e-5 of
// autovacuum_freeze_max_age (2000 at the default), so a test reaches red
// by consuming a few thousand XIDs.
const holderRedBuffer = 99.999

// burnForRed is how many XIDs make a table created before them red.
func burnForRed(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var maxAge int64
	if err := pool.QueryRow(context.Background(),
		"SELECT current_setting('autovacuum_freeze_max_age')::bigint").Scan(&maxAge); err != nil {
		t.Fatalf("read autovacuum_freeze_max_age: %v", err)
	}
	return int(float64(maxAge)*(100-holderRedBuffer)/100) + 600
}

// holderTables creates hfx.<prefix>NN (one row each, autovacuum off,
// frozen).
func holderTables(t *testing.T, pool *pgxpool.Pool, prefix string, n int, with string) []string {
	t.Helper()
	var names []string
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("hfx.%s%02d", prefix, i)
		bloatTestExec(t, pool, fmt.Sprintf(`CREATE TABLE %s (id int)
			WITH (autovacuum_enabled = false%s); INSERT INTO %s VALUES (1)`, name, with, name))
		// A new table's relfrozenxid is the cluster's RecentXmin, which a
		// concurrent test's transaction can date back; freezing dates it
		// from this database's horizon. On the shared server PostgreSQL 14
		// holds that horizon back to the oldest XID running in any database
		// while a backend logs in, so every caller holds the exclusive one.
		bloatTestExec(t, pool, "VACUUM (FREEZE) "+name)
		names = append(names, name)
	}
	return names
}

func holderSchema(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	bloatTestExec(t, pool, "DROP SCHEMA IF EXISTS hfx CASCADE; CREATE SCHEMA hfx")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS hfx CASCADE")
	})
}

func holderScan(t *testing.T, pool *pgxpool.Pool) []Proposal {
	t.Helper()
	proposals, err := NewPostgresFreezeCustodian(pool, "testdb", holderRedBuffer).
		Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	return proposals
}

// fixtureProposals are the proposals of feature that name a hfx table.
func fixtureProposals(proposals []Proposal, feature string) []Proposal {
	var out []Proposal
	for _, p := range proposals {
		for _, target := range p.TargetObjects {
			if p.Feature == feature && strings.HasPrefix(target, "hfx.") {
				out = append(out, p)
				break
			}
		}
	}
	return out
}

func containsAll(have, want []string) bool {
	set := map[string]bool{}
	for _, h := range have {
		set[h] = true
	}
	for _, w := range want {
		if !set[w] {
			return false
		}
	}
	return true
}

// idleWriter leaves a READ COMMITTED session idle in a transaction that
// updated table: it holds backend_xid, and no snapshot (backend_xmin).
func idleWriter(t *testing.T, pool *pgxpool.Pool, table string) int {
	t.Helper()
	ctx := context.Background()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	for _, sql := range []string{"BEGIN ISOLATION LEVEL READ COMMITTED",
		"UPDATE " + table + " SET id = id + 1"} {
		if _, err := conn.Exec(ctx, sql); err != nil {
			conn.Release()
			t.Fatalf("%s: %v", sql, err)
		}
	}
	t.Cleanup(func() {
		_, _ = conn.Exec(context.Background(), "ROLLBACK")
		conn.Release()
	})
	return int(conn.Conn().PgConn().PID())
}

// The test holds the exclusive server: on the shared one PostgreSQL 14
// dates a freeze back to the oldest XID running in any database while a
// backend logs in (CI: a table frozen just before read 161018 XIDs old),
// and the writer's 0.93 share of that table's age can then need more
// than the 2M XIDs a settle step may burn.
func TestFreezeHoldersNameAnIdleWriterByItsXID(t *testing.T) {
	pool := exclusiveAutonomyDB(t, "table XID ages against an idle writer's XID")
	holderSchema(t, pool)
	tables := holderTables(t, pool, "w", 2, "")
	pid := idleWriter(t, pool, tables[0])
	testdb.BurnXIDs(t, context.Background(), pool, burnForRed(t, pool))
	settleShares(t, pool, "SELECT age(backend_xid) FROM pg_stat_activity WHERE pid = $2", pid,
		map[string]shareBand{tables[0]: pinsAll, tables[1]: pinsAll})

	proposals := holderScan(t, pool)

	blockers := fixtureProposals(proposals, "freeze_blocker")
	if len(blockers) != 1 || blockers[0].Evidence["pid"] != pid ||
		!containsAll(blockers[0].TargetObjects, tables) {
		t.Fatalf("holder proposals = %#v, want one naming pid %d on %v", blockers, pid, tables)
	}
	b := blockers[0]
	xidAge, _ := b.Evidence["backend_xid_age"].(int64)
	if b.SQL != fmt.Sprintf("SELECT pg_terminate_backend(%d)", pid) || xidAge <= 0 ||
		b.Evidence["backend_xmin_age"] != nil || b.Evidence["backend_start"] == "" {
		t.Fatalf("idle writer proposal = %#v", b)
	}
	// The writer's XID is about as old as the tables: freezing cannot help.
	if freezes := fixtureProposals(proposals, "freeze"); len(freezes) != 0 {
		t.Fatalf("futile freezes proposed: %#v", freezes)
	}
}

func TestFreezeHoldersOneProposalForFiftyRedTablesLive(t *testing.T) {
	pool := exclusiveAutonomyDB(t, "table XID ages against a held snapshot")
	holderSchema(t, pool)
	tables := holderTables(t, pool, "f", 50, "")
	pid := holdSnapshot(t, pool, "orders_api")
	testdb.BurnXIDs(t, context.Background(), pool, burnForRed(t, pool))
	// The tables were frozen one after another, so the oldest gives the
	// holder its lowest share; the holder must still pin all 50.
	bands := map[string]shareBand{}
	for _, table := range tables {
		bands[table] = pinsAll
	}
	settleShares(t, pool, "SELECT age(backend_xmin) FROM pg_stat_activity WHERE pid = $2",
		pid, bands)

	blockers := fixtureProposals(holderScan(t, pool), "freeze_blocker")

	if len(blockers) != 1 {
		t.Fatalf("holder proposals = %d, want 1 for 50 pinned tables: %#v",
			len(blockers), blockers)
	}
	if blockers[0].Evidence["pid"] != pid || !containsAll(blockers[0].TargetObjects, tables) {
		t.Fatalf("holder proposal = %#v, want pid %d on all 50 tables", blockers[0], pid)
	}
}

// preparedGID prepares a transaction that wrote table and returns the
// md5 of its gid; it skips when the server has no prepared transactions.
func preparedGID(t *testing.T, pool *pgxpool.Pool, table string) string {
	t.Helper()
	ctx := context.Background()
	var slots int
	if err := pool.QueryRow(ctx, "SELECT current_setting('max_prepared_transactions')::int").
		Scan(&slots); err != nil {
		t.Fatalf("read max_prepared_transactions: %v", err)
	}
	if slots == 0 {
		t.Skip("max_prepared_transactions = 0: PREPARE TRANSACTION is unavailable here")
	}
	gid := fmt.Sprintf("vacuum02_%d", time.Now().UnixNano())
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	for _, sql := range []string{"BEGIN", "UPDATE " + table + " SET id = id + 1",
		"PREPARE TRANSACTION '" + gid + "'"} {
		if _, err := conn.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "ROLLBACK PREPARED '"+gid+"'")
	})
	var hash string
	if err := pool.QueryRow(ctx, "SELECT md5($1)", gid).Scan(&hash); err != nil {
		t.Fatalf("hash gid: %v", err)
	}
	return hash
}

// shareBand is the least and the excluded greatest share of a table's
// XID age the holder must reach before the scan.
type shareBand struct{ lo, hi float64 }

// pinsAll and pinsAndFreezes are the two bands under test: a holder so
// old that freezing cannot help, and one that pins while freezing helps.
var (
	pinsAll        = shareBand{0.93, 100}
	pinsAndFreezes = shareBand{0.6, 0.85}
)

// settleShares consumes XIDs until the holder (holderSQL, an age, with
// $2 = arg) is within its band of every table's XID age. It measures each
// table's gap instead of assuming it, burns what the gap still needs, and
// fails when the bands cannot all be met or a step would need more than
// maxSettleBurn. Its callers hold the exclusive server (see holderTables)
// so that each freeze reaches about the XID it ran at.
func settleShares(t *testing.T, pool *pgxpool.Pool, holderSQL string, arg any,
	bands map[string]shareBand) {
	t.Helper()
	for step := 0; step < 20; step++ {
		need, ceiling := 0, maxSettleBurn
		for table, band := range bands {
			var holder, age float64
			if err := pool.QueryRow(context.Background(), `SELECT (`+holderSQL+
				`)::float8, age(relfrozenxid)::float8 FROM pg_class WHERE oid = $1::regclass`,
				table, arg).Scan(&holder, &age); err != nil {
				t.Fatalf("read the holder's share of %s: %v", table, err)
			}
			if holder >= band.hi*age {
				t.Fatalf("fixture drifted: holder is %.2f of %s's age, want under %.2f",
					holder/age, table, band.hi)
			}
			need = max(need, burnToShare(holder, age, band.lo))
			if band.hi < 1 {
				ceiling = min(ceiling, burnToShare(holder, age, band.hi)-1)
			}
		}
		if need == 0 {
			return
		}
		if ceiling < need {
			t.Fatalf("holder cannot reach every band %v: needs %d XIDs, at most %d fit",
				bands, need, ceiling)
		}
		testdb.BurnXIDs(t, context.Background(), pool, min(need+settleMargin, ceiling))
	}
	t.Fatalf("holder never reached its share bands %v", bands)
}

// maxSettleBurn bounds one settle step; settleMargin rounds a step past
// its band's edge.
const (
	maxSettleBurn = 2_000_000
	settleMargin  = 20
)

// burnToShare is how many XIDs raise a holder at holderAge to share of a
// table at tableAge (every XID ages both by one); 0 once it is there.
func burnToShare(holderAge, tableAge, share float64) int {
	if share >= 1 || holderAge >= share*tableAge {
		return 0
	}
	return int(math.Ceil((share*tableAge - holderAge) / (1 - share)))
}

// The test holds the exclusive server: the older table's band
// (pinsAndFreezes) and the younger one's (pinsAll) fit together only while
// each freeze reaches about the XID it ran at, and on the shared server
// PostgreSQL 14 dates a freeze back to the oldest XID running in any
// database while a backend logs in.
func TestFreezeHoldersEscalatePreparedTransactions(t *testing.T) {
	pool := exclusiveAutonomyDB(t, "table XID ages against a prepared transaction")
	holderSchema(t, pool)
	burn := burnForRed(t, pool)
	older := holderTables(t, pool, "old", 1, "")
	testdb.BurnXIDs(t, context.Background(), pool, 3*burn)
	younger := holderTables(t, pool, "new", 1, "")
	hash := preparedGID(t, pool, younger[0])
	testdb.BurnXIDs(t, context.Background(), pool, burn)
	settleShares(t, pool, "SELECT age(transaction) FROM pg_prepared_xacts WHERE md5(gid) = $2",
		hash, map[string]shareBand{older[0]: pinsAndFreezes, younger[0]: pinsAll})

	proposals := holderScan(t, pool)

	blockers := fixtureProposals(proposals, "freeze_blocker")
	if len(blockers) != 1 {
		t.Fatalf("holder proposals = %#v, want one escalation", blockers)
	}
	b := blockers[0]
	if b.SQL != "" || b.ResponseKind != string(freeze.ResponseEscalateHolder) ||
		b.Evidence["holder_kind"] != freeze.HolderPreparedXact ||
		b.Evidence["holder_name"] != hash || !strings.Contains(b.Plan, hash) ||
		!containsAll(b.TargetObjects, append(older, younger...)) {
		t.Fatalf("prepared transaction escalation = %#v (gid hash %s)", b, hash)
	}
	// The older table (holder 0.6 to 0.85 of its age) is still frozen up to
	// the horizon; the younger one (holder about its age) is not.
	freezes := fixtureProposals(proposals, "freeze")
	if len(freezes) != 1 || freezes[0].TargetObjects[0] != older[0] {
		t.Fatalf("freezes = %#v, want %s only", freezes, older[0])
	}
}

// catalogSlots are the slots holding a catalog_xmin.
func catalogSlots(t *testing.T, pool *pgxpool.Pool) map[string]bool {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		"SELECT slot_name::text FROM pg_replication_slots WHERE catalog_xmin IS NOT NULL")
	if err != nil {
		t.Fatalf("list slots: %v", err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("list slots: %v", err)
	}
	set := map[string]bool{}
	for _, name := range names {
		set[name] = true
	}
	return set
}

func TestFreezeHoldersNameALogicalSlotOnCatalogTables(t *testing.T) {
	pool := exclusiveAutonomyDB(t, "catalog XID ages against a logical slot")
	holderSchema(t, pool)
	ctx := context.Background()
	catalog := holderTables(t, pool, "cat", 1, ", user_catalog_table = true")
	plain := holderTables(t, pool, "plain", 1, "")
	slot := "vacuum02_slot"
	_, _ = pool.Exec(ctx, "SELECT pg_drop_replication_slot($1)", slot)
	if _, err := pool.Exec(ctx,
		"SELECT pg_create_logical_replication_slot($1, 'test_decoding')", slot); err != nil {
		t.Fatalf("create logical slot (wal_level=logical is required): %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "SELECT pg_drop_replication_slot($1)", slot)
	})
	testdb.BurnXIDs(t, context.Background(), pool, burnForRed(t, pool))
	settleShares(t, pool, "SELECT age(catalog_xmin) FROM pg_replication_slots "+
		"WHERE slot_name = $2", slot, map[string]shareBand{catalog[0]: pinsAll})

	// Slots are cluster-wide: another package's test may hold an older one,
	// which is then the catalog table's oldest holder.
	slots := catalogSlots(t, pool)

	proposals := holderScan(t, pool)

	blockers := fixtureProposals(proposals, "freeze_blocker")
	if len(blockers) != 1 || blockers[0].Evidence["holder_kind"] != freeze.HolderSlotCatalog ||
		!containsAll(blockers[0].TargetObjects, catalog) ||
		containsAll(blockers[0].TargetObjects, plain) {
		t.Fatalf("slot escalation = %#v, want a catalog_xmin slot on %v only", blockers, catalog)
	}
	named, _ := blockers[0].Evidence["holder_name"].(string)
	if !slots[named] || !strings.Contains(blockers[0].Plan, `"`+named+`"`) || !slots[slot] {
		t.Fatalf("escalation names slot %q (plan %q), want one of %v", named,
			blockers[0].Plan, slots)
	}
	freezes := fixtureProposals(proposals, "freeze")
	if len(freezes) != 1 || freezes[0].TargetObjects[0] != plain[0] {
		t.Fatalf("freezes = %#v, want %s (catalog_xmin does not hold it)", freezes, plain[0])
	}
}

func TestFreezeHoldersQueryErrorAbortsTheScan(t *testing.T) {
	pool := requireAutonomyDB(t)
	saved := xminHoldersSQL
	xminHoldersSQL = "SELECT holder_kind FROM no_such_relation LIMIT $1"
	t.Cleanup(func() { xminHoldersSQL = saved })

	proposals, err := NewPostgresFreezeCustodian(pool, "testdb", 25).Scan(context.Background())

	if err == nil || !strings.Contains(err.Error(), "read xmin holders") ||
		!strings.Contains(err.Error(), "no_such_relation") || proposals != nil {
		t.Fatalf("Scan = %#v, %v; want the holder query error", proposals, err)
	}
}

// A backend running a lazy VACUUM shows a backend_xmin, but its snapshot
// holds back no horizon (PROC_IN_VACUUM): it is never a holder, so an
// operator's VACUUM is never proposed for cancellation.
func TestXminHoldersSkipALazyVacuum(t *testing.T) {
	pool := requireAutonomyDB(t)
	holderSchema(t, pool)
	ctx := context.Background()
	bloatTestExec(t, pool, `CREATE TABLE hfx.slow WITH (autovacuum_enabled = false) AS
		SELECT g AS id FROM generate_series(1, 200000) g; DELETE FROM hfx.slow WHERE id % 2 = 0`)
	pid := slowVacuum(t, pool, "hfx.slow")

	holders, err := readXminHolders(ctx, pool)

	if err != nil {
		t.Fatalf("xminHolders: %v", err)
	}
	if h := holderByPID(holders, pid); h != nil {
		t.Fatalf("lazy VACUUM backend %d listed as a holder: %#v", pid, h)
	}
}

// slowVacuum starts a cost-throttled VACUUM of table on its own
// connection and returns its PID once pg_stat_progress_vacuum lists it
// with a backend_xmin; cleanup cancels it.
func slowVacuum(t *testing.T, pool *pgxpool.Pool, table string) int {
	t.Helper()
	ctx := context.Background()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	pid := int(conn.Conn().PgConn().PID())
	for _, sql := range []string{"SET vacuum_cost_delay = '100ms'", "SET vacuum_cost_limit = 1"} {
		if _, err := conn.Exec(ctx, sql); err != nil {
			conn.Release()
			t.Fatalf("%s: %v", sql, err)
		}
	}
	done := make(chan error, 1)
	go func() { _, err := conn.Exec(ctx, "VACUUM "+table); done <- err }()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "SELECT pg_cancel_backend($1)", pid)
		<-done
		conn.Release()
	})
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		var withXmin bool
		err := pool.QueryRow(ctx, `SELECT a.backend_xmin IS NOT NULL FROM pg_stat_activity a
			JOIN pg_stat_progress_vacuum v ON v.pid = a.pid WHERE a.pid = $1`, pid).Scan(&withXmin)
		if err == nil && withXmin {
			return pid
		}
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("read VACUUM progress of pid %d: %v", pid, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("VACUUM of %s (pid %d) never showed progress with a backend_xmin", table, pid)
	return 0
}
