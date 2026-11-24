package config

import (
	"strings"
	"testing"
	"time"
)

// No concurrency or integration tests: validation is a pure function of
// the config value.

func TestFleetControlDatabase_DefaultsEmpty(t *testing.T) {
	if pin := DefaultConfig().FleetLearning.ControlDatabase; pin != "" {
		t.Fatalf("default control_database = %q, want empty", pin)
	}
}

func TestFleetControlDatabase_NotInDatabasesFailsValidation(t *testing.T) {
	c := DefaultConfig()
	c.FleetLearning.ControlDatabase = "nope"
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(),
		`fleet_learning.control_database "nope" is not a name under the databases key`) {
		t.Fatalf("Validate = %v, want the unknown pin named", err)
	}
}

func TestFleetControlDatabase_Rules(t *testing.T) {
	fleetCfg := func(pin, meta string) *Config {
		c := DefaultConfig()
		c.Databases = []DatabaseConfig{{Name: "db1"}, {Name: "db2"}}
		c.FleetLearning.ControlDatabase, c.MetaDB = pin, meta
		return c
	}
	cases := []struct {
		name, pin, meta, want string
	}{
		{"named database", "db2", "", ""},
		{"unset", "", "", ""},
		{"unset with meta", "", "postgres://meta", ""},
		{"case differs", "DB1", "", `"DB1" is not a name under the databases key`},
		{"spaces", " db1", "", `" db1" is not a name under the databases key`},
		{"with meta_db", "db1", "postgres://meta", "with meta_db the meta database"},
	}
	for _, tc := range cases {
		err := fleetCfg(tc.pin, tc.meta).validateFleetLearning()
		if tc.want == "" && err != nil {
			t.Errorf("%s: %v, want valid", tc.name, err)
		}
		if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%s: %v, want %q", tc.name, err, tc.want)
		}
	}
	c := fleetCfg("db1", "")
	c.FleetLearning.LeaderLeaseSeconds = 5
	if err := c.validateFleetLearning(); err == nil ||
		!strings.Contains(err.Error(), "leader_lease_seconds") {
		t.Fatalf("per-key checks must still run: %v", err)
	}
}

func TestFleetControlDatabase_LoadsFromYAML(t *testing.T) {
	path := wave5WriteYAML(t, "fleet_learning:\n  control_database: db1\n")
	c := DefaultConfig()
	if err := loadYAML(path, c); err != nil {
		t.Fatal(err)
	}
	if c.FleetLearning.ControlDatabase != "db1" {
		t.Fatalf("control_database = %q, want db1", c.FleetLearning.ControlDatabase)
	}
	if class, ok := KeyClassOf("fleet_learning.control_database"); !ok ||
		class != KeyOperatorPreference {
		t.Fatalf("class = %q known=%v, want operator_preference", class, ok)
	}
}

func TestFleetOrphanScopeDays_DefaultAndBounds(t *testing.T) {
	c := DefaultConfig()
	if c.FleetLearning.OrphanScopeDays != 14 ||
		c.FleetLearning.OrphanScopeAge() != 14*24*time.Hour {
		t.Fatalf("default = %d (%v), want 14 days", c.FleetLearning.OrphanScopeDays,
			c.FleetLearning.OrphanScopeAge())
	}
	for _, v := range []int{1, 3650} {
		c.FleetLearning.OrphanScopeDays = v
		if err := c.Validate(); err != nil {
			t.Fatalf("orphan_scope_days %d rejected: %v", v, err)
		}
	}
	for _, v := range []int{0, -1, 3651} {
		c.FleetLearning.OrphanScopeDays = v
		if err := c.Validate(); err == nil ||
			!strings.Contains(err.Error(), "fleet_learning.orphan_scope_days must be 1-3650") {
			t.Fatalf("orphan_scope_days %d: %v, want rejected", v, err)
		}
	}
	if class, ok := KeyClassOf("fleet_learning.orphan_scope_days"); !ok ||
		class != KeyOperatorPreference {
		t.Fatalf("class = %q known=%v", class, ok)
	}
}
