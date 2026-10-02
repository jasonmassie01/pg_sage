package probes

import (
	"strings"
	"testing"
)

// M6 reactive probes: checkpoint storms, temp-file explosions,
// replication lag and LWLock contention. Each is read-only, capped like
// every catalog probe, and covers PostgreSQL 14 to 18 with a variant per
// changed view (pg_stat_bgwriter before 17, pg_stat_checkpointer and
// pg_stat_io from 17).

func m6ReactiveIDs() []ID {
	return []ID{CheckpointActivity, TempFileActivity, TempFileHolders,
		TempSpillStatements, StandbyReplayState, LWLockWaits}
}

func TestCatalog_HasM6ReactiveProbes(t *testing.T) {
	families := map[ID]string{CheckpointActivity: FamilyWAL,
		TempFileActivity: FamilyTempFiles, TempFileHolders: FamilyTempFiles,
		TempSpillStatements: FamilyTempFiles, StandbyReplayState: FamilyReplication,
		LWLockWaits: FamilyWaits}
	for _, id := range m6ReactiveIDs() {
		spec, ok := Catalog().Spec(id)
		if !ok {
			t.Fatalf("catalog lacks %s", id)
		}
		if spec.Family != families[id] || spec.Args != ArgsNone || spec.Version != "v1" {
			t.Errorf("%s = family %q args %d version %s", id, spec.Family, spec.Args,
				spec.Version)
		}
		for _, v := range []int{140000, 150000, 160000, 170000, 180000} {
			if _, ok := spec.VariantFor(v); !ok {
				t.Errorf("%s has no variant for server_version_num %d", id, v)
			}
		}
	}
}

func TestCatalog_CheckpointActivityReadsTheRightViewPerVersion(t *testing.T) {
	spec, _ := Catalog().Spec(CheckpointActivity)
	v16, _ := spec.VariantFor(160009)
	v17, _ := spec.VariantFor(170000)
	v18, _ := spec.VariantFor(180004)
	if !strings.Contains(v16.SQL, "pg_stat_bgwriter") ||
		strings.Contains(v16.SQL, "pg_stat_checkpointer") {
		t.Error("PG16 must read checkpoint counters from pg_stat_bgwriter")
	}
	for _, v := range []Variant{v17, v18} {
		if !strings.Contains(v.SQL, "pg_stat_checkpointer") ||
			!strings.Contains(v.SQL, "pg_stat_io") ||
			strings.Contains(v.SQL, "buffers_backend") {
			t.Error("PG17+ must read pg_stat_checkpointer and backend fsyncs from pg_stat_io")
		}
	}
	for _, v := range spec.Variants {
		for _, gone := range []string{"wal_write_time", "wal_sync_time", "op_bytes"} {
			if strings.Contains(v.SQL, gone) {
				t.Errorf("checkpoint_activity reads %s, which PG18 removed", gone)
			}
		}
	}
}

func TestCatalog_ReplicationLagV2SplitsTheLagByStage(t *testing.T) {
	spec, _ := Catalog().Spec(ReplicationLag)
	if spec.Version != "v2" {
		t.Fatalf("replication_lag version %s, want v2 (stage backlogs added)", spec.Version)
	}
	v, _ := spec.VariantFor(170000)
	for _, col := range []string{"send_backlog_bytes", "flush_backlog_bytes",
		"replay_backlog_bytes", "replay_lag_bytes", "write_lag_s", "kind"} {
		if !strings.Contains(v.SQL, col) {
			t.Errorf("replication_lag v2 lacks %s", col)
		}
	}
}

// pg_stat_statements lives in whichever schema it was created in; the
// probe names the extension and the runner resolves the schema, so the
// probe never depends on search_path.
func TestCatalog_SpillStatementsResolvesTheExtensionSchema(t *testing.T) {
	spec, _ := Catalog().Spec(TempSpillStatements)
	if spec.Extension != "pg_stat_statements" {
		t.Fatalf("temp_spill_statements extension = %q", spec.Extension)
	}
	for _, v := range spec.Variants {
		if !strings.Contains(v.SQL, ExtSchemaToken+".pg_stat_statements") {
			t.Errorf("temp_spill_statements must read %s.pg_stat_statements",
				ExtSchemaToken)
		}
	}
	for _, id := range Catalog().IDs() {
		s, _ := Catalog().Spec(id)
		if s.Extension == "" {
			for _, v := range s.Variants {
				if strings.Contains(v.SQL, ExtSchemaToken) {
					t.Errorf("%s uses %s without naming an extension", id, ExtSchemaToken)
				}
			}
		}
	}
}

func TestNewRegistry_ValidatesExtensionSpecs(t *testing.T) {
	ok := validSpec()
	ok.Extension = "pg_stat_statements"
	ok.Variants = []Variant{{SQL: "SELECT 1 FROM " + ExtSchemaToken +
		".pg_stat_statements LIMIT $1"}}
	if _, err := NewRegistry(ok); err != nil {
		t.Fatalf("valid extension spec rejected: %v", err)
	}
	cases := map[string]func(*Spec){
		"extension without token": func(s *Spec) {
			s.Variants = []Variant{{SQL: "SELECT 1 LIMIT $1"}}
		},
		"token without extension": func(s *Spec) { s.Extension = "" },
		"invalid extension name":  func(s *Spec) { s.Extension = "x; drop" },
		"token in one variant only": func(s *Spec) {
			s.Variants = append(s.Variants, Variant{MinVersion: 170000,
				SQL: "SELECT 2 LIMIT $1"})
		},
	}
	for name, mutate := range cases {
		spec := ok
		spec.Variants = append([]Variant(nil), ok.Variants...)
		mutate(&spec)
		if _, err := NewRegistry(spec); err == nil {
			t.Errorf("NewRegistry accepted %s", name)
		}
	}
}

func TestForSignal_MapsM6LogSignals(t *testing.T) {
	if got := ForSignal("log_checkpoint_too_frequent"); !containsID(got, CheckpointActivity) {
		t.Errorf("log_checkpoint_too_frequent probes = %v", got)
	}
	if got := ForSignal("log_temp_file_created"); !containsID(got, TempFileActivity) ||
		!containsID(got, TempFileHolders) {
		t.Errorf("log_temp_file_created probes = %v", got)
	}
	if got := ForSignal("log_replication_conflict"); !containsID(got, StandbyReplayState) {
		t.Errorf("log_replication_conflict probes = %v", got)
	}
	for _, sig := range []string{"log_checkpoint_too_frequent", "log_temp_file_created",
		"log_replication_conflict"} {
		if n := len(ForSignal(sig)); n == 0 || n > MaxProbesPerSignal {
			t.Errorf("%s maps to %d probes", sig, n)
		}
	}
}
