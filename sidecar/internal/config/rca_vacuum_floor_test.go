package config

import (
	"strings"
	"testing"
)

// Dogfood round 2: RCA's vacuum_blocked signal counts a table only above
// a size floor (rca.vacuum_min_dead_tuples, rca.vacuum_min_table_mb). The
// defaults hold with no file, with a partial rca section and for a
// zero-value RCAConfig literal, so a ratio on a tiny table never fires.

func TestVacuumFloorDefaults_NoConfigFile(t *testing.T) {
	chdirTemp(t)
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RCA.VacuumMinDeadTuples != 1000 || cfg.RCA.VacuumMinTableMB != 8 {
		t.Fatalf("floors = %d dead tuples, %d MB; want 1000 and 8",
			cfg.RCA.VacuumMinDeadTuples, cfg.RCA.VacuumMinTableMB)
	}
	if cfg.RCA.VacuumDeadTupleFloor() != 1000 ||
		cfg.RCA.VacuumTableBytesFloor() != 8<<20 {
		t.Fatalf("accessors = %d, %d", cfg.RCA.VacuumDeadTupleFloor(),
			cfg.RCA.VacuumTableBytesFloor())
	}
	d := DefaultConfig()
	if d.RCA.VacuumMinDeadTuples != 1000 || d.RCA.VacuumMinTableMB != 8 {
		t.Fatal("DefaultConfig and Load(nil) disagree")
	}
}

func TestVacuumFloor_PartialSectionKeepsDefaults(t *testing.T) {
	cfg, err := loadRCAYAML(t, "rca:\n  vacuum_min_dead_tuples: 5000\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RCA.VacuumMinDeadTuples != 5000 || cfg.RCA.VacuumMinTableMB != 8 {
		t.Fatalf("floors = %d, %d MB; want 5000 and the default 8",
			cfg.RCA.VacuumMinDeadTuples, cfg.RCA.VacuumMinTableMB)
	}
	if cfg.RCA.VacuumTableBytesFloor() != 8*1024*1024 {
		t.Fatalf("bytes floor = %d", cfg.RCA.VacuumTableBytesFloor())
	}
}

func TestVacuumFloor_ZeroValueLiteralMeansDefault(t *testing.T) {
	r := &RCAConfig{}
	if r.VacuumDeadTupleFloor() != 1000 || r.VacuumTableBytesFloor() != 8<<20 {
		t.Fatalf("zero-value floors = %d, %d; want the defaults",
			r.VacuumDeadTupleFloor(), r.VacuumTableBytesFloor())
	}
	r = &RCAConfig{VacuumMinDeadTuples: 1, VacuumMinTableMB: 1}
	if r.VacuumDeadTupleFloor() != 1 || r.VacuumTableBytesFloor() != 1<<20 {
		t.Fatalf("explicit floors = %d, %d", r.VacuumDeadTupleFloor(),
			r.VacuumTableBytesFloor())
	}
}

func TestVacuumFloor_Bounds(t *testing.T) {
	cases := []struct {
		name, body, wantErr string
	}{
		{"dead tuples minimum", "rca:\n  vacuum_min_dead_tuples: 1\n", ""},
		{"dead tuples zero", "rca:\n  vacuum_min_dead_tuples: 0\n",
			"rca.vacuum_min_dead_tuples must be 1-1000000000, got 0"},
		{"dead tuples negative", "rca:\n  vacuum_min_dead_tuples: -5\n",
			"rca.vacuum_min_dead_tuples must be 1-1000000000, got -5"},
		{"dead tuples maximum", "rca:\n  vacuum_min_dead_tuples: 1000000000\n", ""},
		{"dead tuples above maximum", "rca:\n  vacuum_min_dead_tuples: 1000000001\n",
			"rca.vacuum_min_dead_tuples must be 1-1000000000, got 1000000001"},
		{"table mb minimum", "rca:\n  vacuum_min_table_mb: 1\n", ""},
		{"table mb zero", "rca:\n  vacuum_min_table_mb: 0\n",
			"rca.vacuum_min_table_mb must be 1-1048576, got 0"},
		{"table mb maximum", "rca:\n  vacuum_min_table_mb: 1048576\n", ""},
		{"table mb above maximum", "rca:\n  vacuum_min_table_mb: 1048577\n",
			"rca.vacuum_min_table_mb must be 1-1048576, got 1048577"},
		{"table mb not a number", "rca:\n  vacuum_min_table_mb: lots\n", "lots"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadRCAYAML(t, tc.body)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Load rejected a valid floor: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Load error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}
