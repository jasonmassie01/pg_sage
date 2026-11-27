package config

import (
	"strings"
	"testing"
	"time"
)

// rca-01: the vacuum_blocked tree names an xmin holder only when its
// transaction is older than rca.xmin_holder_min_minutes, and a prepared
// transaction only when it is older than rca.prepared_xact_min_age_minutes.
// The defaults hold with no file, a partial section and a zero literal.

// No concurrent access test: the accessors read two ints of a value.

func TestHolderFloorDefaults_NoConfigFile(t *testing.T) {
	chdirTemp(t)
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RCA.XminHolderMinMinutes != 5 || cfg.RCA.PreparedXactMinAgeMinutes != 30 {
		t.Fatalf("floors = %d, %d minutes; want 5 and 30",
			cfg.RCA.XminHolderMinMinutes, cfg.RCA.PreparedXactMinAgeMinutes)
	}
	if cfg.RCA.XminHolderFloor() != 5*time.Minute ||
		cfg.RCA.PreparedXactFloor() != 30*time.Minute {
		t.Fatalf("accessors = %s, %s", cfg.RCA.XminHolderFloor(),
			cfg.RCA.PreparedXactFloor())
	}
	d := DefaultConfig()
	if d.RCA.XminHolderMinMinutes != 5 || d.RCA.PreparedXactMinAgeMinutes != 30 {
		t.Fatal("DefaultConfig and Load(nil) disagree")
	}
}

func TestHolderFloor_PartialSectionKeepsDefaults(t *testing.T) {
	cfg, err := loadRCAYAML(t, "rca:\n  xmin_holder_min_minutes: 12\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RCA.XminHolderFloor() != 12*time.Minute ||
		cfg.RCA.PreparedXactFloor() != 30*time.Minute {
		t.Fatalf("floors = %s, %s; want 12m and the default 30m",
			cfg.RCA.XminHolderFloor(), cfg.RCA.PreparedXactFloor())
	}
}

func TestHolderFloor_ZeroValueLiteralMeansDefault(t *testing.T) {
	r := &RCAConfig{}
	if r.XminHolderFloor() != 5*time.Minute || r.PreparedXactFloor() != 30*time.Minute {
		t.Fatalf("zero-value floors = %s, %s; want the defaults",
			r.XminHolderFloor(), r.PreparedXactFloor())
	}
	r = &RCAConfig{XminHolderMinMinutes: -3, PreparedXactMinAgeMinutes: -1}
	if r.XminHolderFloor() != 5*time.Minute || r.PreparedXactFloor() != 30*time.Minute {
		t.Fatalf("negative floors = %s, %s; want the defaults",
			r.XminHolderFloor(), r.PreparedXactFloor())
	}
	r = &RCAConfig{XminHolderMinMinutes: 1, PreparedXactMinAgeMinutes: 1}
	if r.XminHolderFloor() != time.Minute || r.PreparedXactFloor() != time.Minute {
		t.Fatalf("explicit floors = %s, %s", r.XminHolderFloor(), r.PreparedXactFloor())
	}
}

func TestHolderFloor_Bounds(t *testing.T) {
	cases := []struct {
		name, body, wantErr string
	}{
		{"holder minimum", "rca:\n  xmin_holder_min_minutes: 1\n", ""},
		{"holder zero", "rca:\n  xmin_holder_min_minutes: 0\n",
			"rca.xmin_holder_min_minutes must be 1-1440, got 0"},
		{"holder negative", "rca:\n  xmin_holder_min_minutes: -1\n",
			"rca.xmin_holder_min_minutes must be 1-1440, got -1"},
		{"holder maximum", "rca:\n  xmin_holder_min_minutes: 1440\n", ""},
		{"holder above maximum", "rca:\n  xmin_holder_min_minutes: 1441\n",
			"rca.xmin_holder_min_minutes must be 1-1440, got 1441"},
		{"prepared minimum", "rca:\n  prepared_xact_min_age_minutes: 1\n", ""},
		{"prepared zero", "rca:\n  prepared_xact_min_age_minutes: 0\n",
			"rca.prepared_xact_min_age_minutes must be 1-10080, got 0"},
		{"prepared maximum", "rca:\n  prepared_xact_min_age_minutes: 10080\n", ""},
		{"prepared above maximum", "rca:\n  prepared_xact_min_age_minutes: 10081\n",
			"rca.prepared_xact_min_age_minutes must be 1-10080, got 10081"},
		{"prepared not a number", "rca:\n  prepared_xact_min_age_minutes: soon\n", "soon"},
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
