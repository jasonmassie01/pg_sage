package rca

import (
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// rca-01: the vacuum_blocked tree names the oldest holder of this
// database's xmin horizon (probes.XminHorizonSQL's holders) whose
// transaction is older than rca.xmin_holder_min_minutes, and proposes
// pg_terminate_backend only for a session idle in transaction for at
// least lock_chain.idle_in_tx_terminate_minutes.

// No concurrent access test: treeVacuumBlocked reads one snapshot and
// keeps no state.

var holderNow = time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

func testVacuumRules() vacuumRules {
	return vacuumRules{holderFloor: 5 * time.Minute, preparedFloor: 30 * time.Minute,
		idleTerminate: 5 * time.Minute}
}

func sessionAt(pid int64, xminAge float64, xact time.Duration, state string) probes.XminHolder {
	return probes.XminHolder{Kind: probes.HolderSession, PID: pid,
		BackendStart: holderNow.Add(-3 * time.Hour), State: state, XminAge: xminAge,
		XactAgeS: xact.Seconds(), Application: "app"}
}

// sessionLock is the session's lock-graph row: in state for idleFor.
func sessionLock(pid int, xact, idleFor time.Duration, state string) collector.LockInfo {
	start := holderNow.Add(-3 * time.Hour)
	xs := holderNow.Add(-xact)
	sc := holderNow.Add(-idleFor)
	return collector.LockInfo{LockType: "virtualxid", Mode: "ExclusiveLock", Granted: true,
		PID: pid, State: strPtr(state), BackendStart: &start, XactStart: &xs,
		StateChange: &sc}
}

func holderSnapshot(holders []probes.XminHolder, locks ...collector.LockInfo) *collector.Snapshot {
	return &collector.Snapshot{CollectedAt: holderNow, XminHolders: holders, Locks: locks}
}

func holderSignal(tables ...string) *Signal {
	if len(tables) == 0 {
		tables = []string{"public.orders"}
	}
	return &Signal{ID: "vacuum_blocked", FiredAt: holderNow, Severity: "warning",
		Metrics: map[string]any{"blocked_tables": tables}}
}

func blockedHolder(t *testing.T, holders []probes.XminHolder,
	locks ...collector.LockInfo) Incident {
	t.Helper()
	return newTestEngine().treeVacuumBlocked(holderSignal(),
		holderSnapshot(holders, locks...), testVacuumRules())
}

func linkBlocker(t *testing.T, inc Incident) *BlockerIdentity {
	t.Helper()
	if len(inc.CausalChain) != 1 {
		t.Fatalf("chain = %+v, want one vacuum_blocked link", inc.CausalChain)
	}
	return inc.CausalChain[0].Blocker
}

func assertBranch3(t *testing.T, inc Incident) {
	t.Helper()
	if !strings.Contains(inc.RootCause, "Autovacuum falling behind") ||
		inc.RecommendedSQL != "" || inc.ActionRisk != "safe" {
		t.Fatalf("incident %q sql %q risk %q, want Branch 3 (no blocker named)",
			inc.RootCause, inc.RecommendedSQL, inc.ActionRisk)
	}
	if _, ok := backendSubject(&inc); ok {
		t.Fatalf("Branch 3 incident names a backend: %+v", inc.CausalChain)
	}
}

func assertNoSignal(t *testing.T, inc Incident) {
	t.Helper()
	for _, fn := range []string{"pg_terminate_backend", "pg_cancel_backend",
		"ROLLBACK PREPARED", "pg_drop_replication_slot"} {
		if strings.Contains(inc.RecommendedSQL, fn) {
			t.Fatalf("recommended SQL %q proposes %s, want a diagnostic only",
				inc.RecommendedSQL, fn)
		}
	}
	if inc.ActionRisk != "safe" {
		t.Fatalf("action risk %q, want safe for a diagnostic", inc.ActionRisk)
	}
}

// Happy path: of two idle holders, the one holding the oldest xmin is
// named with its xmin age, transaction start and a guarded terminate.
func TestVacuumBlockedHolder_NamesOldestHolderWithItsXminAge(t *testing.T) {
	inc := blockedHolder(t, []probes.XminHolder{
		sessionAt(101, 9000, 2*time.Hour, "idle in transaction"),
		sessionAt(102, 500, 10*time.Minute, "idle in transaction"),
	}, sessionLock(102, 10*time.Minute, 10*time.Minute, "idle in transaction"),
		sessionLock(101, 2*time.Hour, time.Hour, "idle in transaction"))
	if !strings.Contains(inc.RootCause, "PID 101") || strings.Contains(inc.RootCause, "102") ||
		!strings.Contains(inc.RootCause, "9000") {
		t.Fatalf("root cause %q, want PID 101 and its xmin age 9000", inc.RootCause)
	}
	b := linkBlocker(t, inc)
	if b == nil || b.PID != 101 || b.XminAge != 9000 || b.HolderKind != "session" ||
		!b.XactStart.Equal(holderNow.Add(-2*time.Hour)) ||
		!b.BackendStart.Equal(holderNow.Add(-3*time.Hour)) || b.State != "idle in transaction" {
		t.Fatalf("blocker = %+v, want pid 101, xmin age 9000, its xact and backend start", b)
	}
	if inc.RecommendedSQL != "SELECT pg_terminate_backend(101); -- idle-in-tx xmin holder" ||
		inc.ActionRisk != "high_risk" {
		t.Fatalf("sql %q risk %q, want terminate of 101 at high_risk",
			inc.RecommendedSQL, inc.ActionRisk)
	}
	if ref, ok := backendSubject(&inc); !ok || ref.PID != 101 {
		t.Fatalf("subject = %+v %v, want pid 101", ref, ok)
	}
}

// A young session tied on xmin age with the real holder (its snapshot
// took the old transaction's xid as xmin) is not the cause: the tree
// names the old one.
func TestVacuumBlockedHolder_SkipsAYoungSessionTiedOnXmin(t *testing.T) {
	inc := blockedHolder(t, []probes.XminHolder{
		sessionAt(201, 9000, 3*time.Second, "active"),
		sessionAt(202, 9000, time.Hour, "idle in transaction"),
	}, sessionLock(202, time.Hour, time.Hour, "idle in transaction"))
	if b := linkBlocker(t, inc); b == nil || b.PID != 202 {
		t.Fatalf("blocker = %+v (root cause %q), want the old session 202", b, inc.RootCause)
	}
}

// An active holder gets a diagnostic only, never a signal.
func TestVacuumBlockedHolder_ActiveHolderGetsADiagnostic(t *testing.T) {
	inc := blockedHolder(t, []probes.XminHolder{sessionAt(301, 7000, time.Hour, "active")},
		sessionLock(301, time.Hour, time.Hour, "active"))
	if !strings.Contains(inc.RootCause, "PID 301") ||
		strings.Contains(inc.RootCause, "Idle-in-transaction") {
		t.Fatalf("root cause %q, want PID 301 named as an active holder", inc.RootCause)
	}
	assertNoSignal(t, inc)
	if !strings.Contains(inc.RecommendedSQL, "pg_stat_activity") ||
		!strings.Contains(inc.RecommendedSQL, "pid = 301") {
		t.Fatalf("diagnostic %q, want a pg_stat_activity read of pid 301", inc.RecommendedSQL)
	}
}

// Nil/empty: no holders, holders with an unknown (NULL) xmin, and only
// young holders all go to Branch 3.
func TestVacuumBlockedHolder_NoUsableHolderIsBranch3(t *testing.T) {
	idleTx := "idle in transaction"
	idle := sessionLock(401, time.Second, time.Second, idleTx)
	for name, snap := range map[string]*collector.Snapshot{
		"nil holders":   holderSnapshot(nil),
		"empty holders": holderSnapshot([]probes.XminHolder{}),
		"null xmin": holderSnapshot([]probes.XminHolder{
			sessionAt(401, 0, time.Hour, "idle")}),
		"young holder": holderSnapshot([]probes.XminHolder{
			sessionAt(401, 50, time.Second, idleTx)}, idle),
		"lock row without age": holderSnapshot(nil,
			collector.LockInfo{PID: 402, State: strPtr(idleTx)}),
	} {
		t.Run(name, func(t *testing.T) {
			assertBranch3(t, newTestEngine().treeVacuumBlocked(holderSignal(), snap,
				testVacuumRules()))
		})
	}
}

// Boundary: the holder floor is inclusive.
func TestVacuumBlockedHolder_FloorBoundary(t *testing.T) {
	for _, tc := range []struct {
		age   time.Duration
		named bool
	}{{5*time.Minute - time.Second, false}, {5 * time.Minute, true},
		{5*time.Minute + time.Second, true}} {
		inc := blockedHolder(t, []probes.XminHolder{sessionAt(501, 800, tc.age, "active")})
		if named := strings.Contains(inc.RootCause, "PID 501"); named != tc.named {
			t.Errorf("xact age %s: named = %v (%q), want %v", tc.age, named, inc.RootCause,
				tc.named)
		}
	}
}

// Boundary: terminate needs the idle time at least the terminate minutes.
func TestVacuumBlockedHolder_IdleTerminateBoundary(t *testing.T) {
	for _, tc := range []struct {
		idle      time.Duration
		terminate bool
	}{{5*time.Minute - time.Second, false}, {5 * time.Minute, true},
		{5*time.Minute + time.Second, true}} {
		inc := blockedHolder(t,
			[]probes.XminHolder{sessionAt(601, 800, time.Hour, "idle in transaction")},
			sessionLock(601, time.Hour, tc.idle, "idle in transaction"))
		if !strings.Contains(inc.RootCause, "PID 601") {
			t.Fatalf("idle %s: root cause %q, want PID 601", tc.idle, inc.RootCause)
		}
		got := strings.Contains(inc.RecommendedSQL, "pg_terminate_backend(601)")
		if got != tc.terminate {
			t.Errorf("idle %s: sql %q, terminate = %v, want %v", tc.idle,
				inc.RecommendedSQL, got, tc.terminate)
		}
		if !tc.terminate {
			assertNoSignal(t, inc)
		}
	}
}

// Fail closed: without the idle time (no lock row), from the lock rows
// alone (the holder read is missing, so slots were not seen), or for a
// protected application, the idle holder is named but not terminated.
func TestVacuumBlockedHolder_TerminateFailsClosed(t *testing.T) {
	dump := sessionAt(703, 800, time.Hour, "idle in transaction")
	dump.Application = "pg_dump"
	safe := sessionAt(704, 800, time.Hour, "idle in transaction")
	safe.Application = "nightly-etl"
	age := int64(800)
	fallback := sessionLock(702, time.Hour, time.Hour, "idle in transaction")
	fallback.BackendXminAge = &age
	rules := testVacuumRules()
	rules.safePatterns = []string{"etl"}
	for name, snap := range map[string]*collector.Snapshot{
		"no lock row": holderSnapshot([]probes.XminHolder{
			sessionAt(701, 800, time.Hour, "idle in transaction")}),
		"lock rows only": holderSnapshot(nil, fallback),
		"pg_dump": holderSnapshot([]probes.XminHolder{dump},
			sessionLock(703, time.Hour, time.Hour, "idle in transaction")),
		"safe pattern": holderSnapshot([]probes.XminHolder{safe},
			sessionLock(704, time.Hour, time.Hour, "idle in transaction")),
	} {
		t.Run(name, func(t *testing.T) {
			inc := newTestEngine().treeVacuumBlocked(holderSignal(), snap, rules)
			if b := linkBlocker(t, inc); b == nil || b.PID < 701 || b.PID > 704 {
				t.Fatalf("blocker = %+v (%q), want the idle holder named", b, inc.RootCause)
			}
			assertNoSignal(t, inc)
		})
	}
}

// A replication slot's xmin older than every session is named as the
// slot; no session is blamed and nothing is signalled.
func TestVacuumBlockedHolder_SlotXminIsNamedAsTheSlot(t *testing.T) {
	inc := blockedHolder(t, []probes.XminHolder{
		{Kind: probes.HolderSlot, Name: "standby_a", State: "inactive", XminAge: 50000},
		sessionAt(801, 900, time.Hour, "idle in transaction"),
	}, sessionLock(801, time.Hour, time.Hour, "idle in transaction"))
	if !strings.Contains(inc.RootCause, `"standby_a"`) || strings.Contains(inc.RootCause, "801") {
		t.Fatalf("root cause %q, want the slot standby_a and no session", inc.RootCause)
	}
	if _, ok := backendSubject(&inc); ok {
		t.Fatalf("slot incident names a backend: %+v", inc.CausalChain)
	}
	b := linkBlocker(t, inc)
	if b == nil || b.HolderKind != "slot" || b.HolderName != "standby_a" || b.XminAge != 50000 {
		t.Fatalf("blocker = %+v, want the slot's structured identity", b)
	}
	assertNoSignal(t, inc)
	if !strings.Contains(inc.RecommendedSQL, "pg_replication_slots") ||
		!strings.Contains(inc.RecommendedSQL, "'standby_a'") {
		t.Fatalf("diagnostic %q, want a quoted pg_replication_slots read", inc.RecommendedSQL)
	}
}

// A slot's catalog_xmin holds back only the system catalogs: it is not
// blamed for user tables, and is for a catalog table.
func TestVacuumBlockedHolder_CatalogXminOnlyForCatalogTables(t *testing.T) {
	holders := []probes.XminHolder{
		{Kind: probes.HolderSlotCatalog, Name: "cdc", State: "active", XminAge: 90000,
			Active: true},
		sessionAt(901, 900, time.Hour, "active"),
	}
	eng := newTestEngine()
	user := eng.treeVacuumBlocked(holderSignal("public.orders"),
		holderSnapshot(holders), testVacuumRules())
	if strings.Contains(user.RootCause, "cdc") || !strings.Contains(user.RootCause, "PID 901") {
		t.Fatalf("user-table root cause %q, want session 901, not the slot", user.RootCause)
	}
	cat := eng.treeVacuumBlocked(holderSignal("pg_catalog.pg_attribute"),
		holderSnapshot(holders), testVacuumRules())
	if !strings.Contains(cat.RootCause, `"cdc"`) ||
		!strings.Contains(cat.RootCause, "catalog_xmin") {
		t.Fatalf("catalog root cause %q, want the slot cdc's catalog_xmin", cat.RootCause)
	}
	assertNoSignal(t, cat)
}

// A standby's hot_standby_feedback is named through its walsender, with a
// diagnostic only.
func TestVacuumBlockedHolder_StandbyFeedbackIsNamed(t *testing.T) {
	inc := blockedHolder(t, []probes.XminHolder{
		{Kind: probes.HolderStandby, PID: 77, State: "streaming", XminAge: 40000},
	})
	if !strings.Contains(inc.RootCause, "standby") || !strings.Contains(inc.RootCause, "77") {
		t.Fatalf("root cause %q, want the standby's walsender 77", inc.RootCause)
	}
	assertNoSignal(t, inc)
	if !strings.Contains(inc.RecommendedSQL, "pg_stat_replication") {
		t.Fatalf("diagnostic %q, want a pg_stat_replication read", inc.RecommendedSQL)
	}
}

// vacuumRules reads the floors from rca and the terminate minutes and safe
// patterns from lock_chain; zero values are the defaults.
func TestVacuumRules_FromConfig(t *testing.T) {
	eng := newTestEngine()
	r := eng.vacuumRules(&config.Config{})
	if r.holderFloor != 5*time.Minute || r.preparedFloor != 30*time.Minute ||
		r.idleTerminate != 5*time.Minute {
		t.Fatalf("zero config rules = %+v, want 5m, 30m and 5m", r)
	}
	r = eng.vacuumRules(nil)
	if r.idleTerminate != 5*time.Minute {
		t.Fatalf("nil config terminate = %s, want the 5m default", r.idleTerminate)
	}
	eng.cfg.XminHolderMinMinutes, eng.cfg.PreparedXactMinAgeMinutes = 7, 45
	cfg := &config.Config{}
	cfg.Analyzer.LockChain.IdleInTxTerminateMinutes = 9
	cfg.Analyzer.LockChain.SafePatterns = []string{"etl"}
	r = eng.vacuumRules(cfg)
	if r.holderFloor != 7*time.Minute || r.preparedFloor != 45*time.Minute ||
		r.idleTerminate != 9*time.Minute || len(r.safePatterns) != 1 ||
		r.safePatterns[0] != "etl" {
		t.Fatalf("configured rules = %+v", r)
	}
}
