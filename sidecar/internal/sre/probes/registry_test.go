package probes

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

// R1 probe catalog (AI-SRE-SPEC §4/§6, Codex §8): a fixed, versioned set
// of read-only probes with hard caps. The model and callers can only name
// a catalog id; they never supply SQL.

// M2 adds the archiver (WAL family: archiver failure) and sage_actions
// ("did pg_sage cause this?", CHECK-38) to the R1 set. M6 adds the runway
// probes: the XID runway, per-table freeze horizons, xmin-horizon holders,
// logged autovacuum cancellations, WAL position and directory, sequences
// and the sampled runway trends.
func r1IDs() []ID {
	return []ID{LockChains, LockGraph, LongTransactions, PreparedXacts,
		BackendIdentity, ConnectionSaturation, ReplicationLag,
		ReplicationSlots, WALCheckpoint, AutovacuumWraparound,
		VacuumProgress, PlanRegressions, Archiver, SageActions,
		XIDRunwayProbe, WraparoundTablesProbe, XminHorizon, AutovacuumCancellations,
		WALRunwayProbe, WALDirectoryProbe, SequenceRunwayProbe, RunwayTrendsProbe,
		ClusterDatabaseSizeProbe}
}

// specVersion is each probe's expected version: connection_saturation
// is v3 (M2 added the server start time, the follow-ups the full server
// identity: a restart or failover between two samples invalidates the
// comparison, CHECK-07), replication_slots and wal_checkpoint are v2
// (the same identity); replication_lag is v2 since M6 split the lag into
// send, flush and replay backlogs; wal_runway is v2 since the fleet dedupe
// moved the databases' size to cluster_database_size; sequence_runway is v2
// since dogfood lifeos-1 bounded it and added its coverage counts.
func specVersion(id ID) string {
	switch id {
	case ConnectionSaturation:
		return "v3"
	case SequenceRunwayProbe:
		return "v3" // v3: per-statement cap, catalog slices
	case ReplicationLag, ReplicationSlots, WALCheckpoint, WALRunwayProbe,
		WraparoundTablesProbe, RunwayTrendsProbe:
		return "v2"
	case LockChains, LockGraph, LongTransactions, LWLockWaits, TempSpillStatements:
		return "v2" // v2: pg_sage's own sessions and statements left out
	}
	return "v1"
}

func TestCatalog_HasEveryR1FamilyWithinCeilings(t *testing.T) {
	reg := Catalog()
	ids := reg.IDs()
	all := append(append(r1IDs(), m6ReactiveIDs()...), statViewIDs()...)
	if len(ids) != len(all) {
		t.Fatalf("catalog ids = %v, want %d probes", ids, len(all))
	}
	families := map[string]bool{}
	for _, id := range all {
		spec, ok := reg.Spec(id)
		if !ok {
			t.Fatalf("catalog lacks %s", id)
		}
		families[spec.Family] = true
		if spec.Version != specVersion(id) {
			t.Errorf("%s version = %q, want %s", id, spec.Version, specVersion(id))
		}
		if spec.StatementTimeout <= 0 || spec.StatementTimeout > MaxStatementTimeout {
			t.Errorf("%s statement timeout %s outside (0, %s]", id,
				spec.StatementTimeout, MaxStatementTimeout)
		}
		if spec.LockTimeout <= 0 || spec.LockTimeout >= spec.StatementTimeout {
			t.Errorf("%s lock timeout %s must be in (0, statement timeout)",
				id, spec.LockTimeout)
		}
		if spec.MaxRows <= 0 || spec.MaxRows > MaxRows {
			t.Errorf("%s max rows %d outside (0, %d]", id, spec.MaxRows, MaxRows)
		}
		if spec.MaxBytes <= 0 || spec.MaxBytes > MaxBytes {
			t.Errorf("%s max bytes %d outside (0, %d]", id, spec.MaxBytes, MaxBytes)
		}
		for _, v := range spec.Variants {
			if !strings.Contains(v.SQL, "LIMIT $1") {
				t.Errorf("%s SQL does not bound rows with LIMIT $1", id)
			}
			if strings.Contains(strings.ToLower(v.SQL), "pg_sleep") {
				t.Errorf("%s SQL calls pg_sleep", id)
			}
		}
	}
	for _, fam := range []string{FamilyLocks, FamilyConnections,
		FamilyReplication, FamilyWAL, FamilyVacuum, FamilyPlans, FamilyChange,
		FamilyTempFiles, FamilyWaits, FamilySequences, FamilyRunway, FamilyStats} {
		if !families[fam] {
			t.Errorf("no probe covers family %s", fam)
		}
	}
}

// Probes never return raw query text or other free text that could carry
// literals: only identities, states, counts and ages.
func TestCatalog_NeverSelectsQueryText(t *testing.T) {
	queryColumn := regexp.MustCompile(`(?i)(\.|[\s,(])query\b`)
	for _, id := range Catalog().IDs() {
		spec, _ := Catalog().Spec(id)
		for _, v := range spec.Variants {
			// The self-exclusion predicate reads the text, never returns it.
			sql := strings.ReplaceAll(v.SQL, notSelfStatement("s.query"), "")
			if m := queryColumn.FindString(sql); m != "" {
				t.Errorf("%s selects query text (%q)", id, m)
			}
		}
	}
}

func validSpec() Spec {
	return Spec{ID: "test_probe", Version: "v1", Family: FamilyLocks,
		Variants:         []Variant{{SQL: "SELECT 1 AS one LIMIT $1"}},
		StatementTimeout: 200 * time.Millisecond, LockTimeout: 50 * time.Millisecond,
		MaxRows: 10, MaxBytes: 1024}
}

func TestNewRegistry_RejectsInvalidSpecs(t *testing.T) {
	cases := map[string]func(*Spec){
		"empty id":            func(s *Spec) { s.ID = "" },
		"id with sql chars":   func(s *Spec) { s.ID = "x; drop" },
		"empty version":       func(s *Spec) { s.Version = "" },
		"unknown family":      func(s *Spec) { s.Family = "shell" },
		"no variants":         func(s *Spec) { s.Variants = nil },
		"empty sql":           func(s *Spec) { s.Variants = []Variant{{SQL: " "}} },
		"zero statement":      func(s *Spec) { s.StatementTimeout = 0 },
		"statement over cap":  func(s *Spec) { s.StatementTimeout = MaxStatementTimeout + 1 },
		"zero lock timeout":   func(s *Spec) { s.LockTimeout = 0 },
		"lock over statement": func(s *Spec) { s.LockTimeout = s.StatementTimeout },
		"zero rows":           func(s *Spec) { s.MaxRows = 0 },
		"rows over cap":       func(s *Spec) { s.MaxRows = MaxRows + 1 },
		"negative bytes":      func(s *Spec) { s.MaxBytes = -1 },
		"bytes over cap":      func(s *Spec) { s.MaxBytes = MaxBytes + 1 },
		"variants not ordered": func(s *Spec) {
			s.Variants = []Variant{{MinVersion: 170000, SQL: "SELECT 1 LIMIT $1"},
				{MinVersion: 140000, SQL: "SELECT 2 LIMIT $1"}}
		},
		"unknown args kind": func(s *Spec) { s.Args = ArgKind(99) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			spec := validSpec()
			mutate(&spec)
			if _, err := NewRegistry(spec); err == nil {
				t.Fatalf("NewRegistry accepted %s", name)
			}
		})
	}
}

func TestNewRegistry_RejectsDuplicateIDs(t *testing.T) {
	if _, err := NewRegistry(validSpec(), validSpec()); err == nil ||
		!strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("err = %v, want duplicate id error", err)
	}
}

func TestNewRegistry_EmptyIsValidButKnowsNothing(t *testing.T) {
	reg, err := NewRegistry()
	if err != nil {
		t.Fatalf("empty registry: %v", err)
	}
	if len(reg.IDs()) != 0 {
		t.Fatalf("ids = %v, want none", reg.IDs())
	}
	if _, ok := reg.Spec(LockGraph); ok {
		t.Fatal("empty registry resolved lock_graph")
	}
}

func TestRegistry_UnknownAndNilLookups(t *testing.T) {
	if _, ok := Catalog().Spec("select_star"); ok {
		t.Fatal("catalog resolved an unknown probe id")
	}
	var nilReg *Registry
	if _, ok := nilReg.Spec(LockGraph); ok || len(nilReg.IDs()) != 0 {
		t.Fatal("nil registry must know no probes")
	}
}

func TestSpec_VariantForVersion(t *testing.T) {
	spec, _ := Catalog().Spec(WALCheckpoint)
	v16, ok16 := spec.VariantFor(160004)
	v17, ok17 := spec.VariantFor(170010)
	if !ok16 || !ok17 {
		t.Fatalf("wal_checkpoint must support PG16 and PG17 (%v %v)", ok16, ok17)
	}
	if v16.SQL == v17.SQL || !strings.Contains(v17.SQL, "pg_stat_checkpointer") ||
		!strings.Contains(v16.SQL, "pg_stat_bgwriter") {
		t.Fatal("wal_checkpoint must read pg_stat_checkpointer on 17+ and " +
			"pg_stat_bgwriter before")
	}
	if _, ok := spec.VariantFor(130000); ok {
		t.Fatal("PG13 is below every variant and must be unsupported")
	}
	if _, ok := spec.VariantFor(0); ok {
		t.Fatal("unknown server version must be unsupported")
	}
}

func TestArgs_ValidateByKind(t *testing.T) {
	start := time.Now()
	cases := []struct {
		name string
		kind ArgKind
		args Args
		ok   bool
	}{
		{"none empty", ArgsNone, Args{}, true},
		{"none with pid", ArgsNone, Args{PID: 5}, false},
		{"backend ok", ArgsBackend, Args{PID: 5, BackendStart: start}, true},
		{"backend zero pid", ArgsBackend, Args{BackendStart: start}, false},
		{"backend negative pid", ArgsBackend, Args{PID: -1, BackendStart: start}, false},
		{"backend no start", ArgsBackend, Args{PID: 5}, false},
		{"window default", ArgsWindow, Args{}, true},
		{"window min", ArgsWindow, Args{Window: MinWindow}, true},
		{"window below min", ArgsWindow, Args{Window: MinWindow - time.Second}, false},
		{"window max", ArgsWindow, Args{Window: MaxWindow}, true},
		{"window above max", ArgsWindow, Args{Window: MaxWindow + time.Second}, false},
		{"window negative", ArgsWindow, Args{Window: -time.Hour}, false},
	}
	for _, c := range cases {
		err := c.args.validate(c.kind)
		if (err == nil) != c.ok {
			t.Errorf("%s: err = %v, want ok=%v", c.name, err, c.ok)
		}
	}
}

func TestForSignal_MapsIncidentSignalsToCatalogProbes(t *testing.T) {
	lock := ForSignal("lock_contention")
	for _, want := range []ID{LockGraph, LockChains, LongTransactions, PreparedXacts} {
		if !containsID(lock, want) {
			t.Errorf("lock_contention probes %v lack %s", lock, want)
		}
	}
	if got := ForSignal("connections_high"); !containsID(got, ConnectionSaturation) {
		t.Errorf("connections_high probes = %v", got)
	}
	if got := ForSignal("replication_lag_increasing"); !containsID(got, ReplicationLag) {
		t.Errorf("replication_lag_increasing probes = %v", got)
	}
	if got := ForSignal("wal_growth_spike"); !containsID(got, WALCheckpoint) {
		t.Errorf("wal_growth_spike probes = %v", got)
	}
	if got := ForSignal("vacuum_blocked"); !containsID(got, AutovacuumWraparound) {
		t.Errorf("vacuum_blocked probes = %v", got)
	}
	if got := ForSignal("no_such_signal"); len(got) != 0 {
		t.Errorf("unknown signal probes = %v, want none", got)
	}
	for _, sig := range []string{"lock_contention", "connections_high",
		"idle_in_tx_elevated", "orphaned_prepared_tx", "vacuum_blocked",
		"replication_lag_increasing", "wal_growth_spike"} {
		got := ForSignal(sig)
		if len(got) == 0 || len(got) > MaxProbesPerSignal {
			t.Errorf("%s maps to %d probes, want 1..%d", sig, len(got),
				MaxProbesPerSignal)
		}
		for _, id := range got {
			if _, ok := Catalog().Spec(id); !ok {
				t.Errorf("%s maps to unknown probe %s", sig, id)
			}
		}
	}
}

func containsID(ids []ID, want ID) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}
