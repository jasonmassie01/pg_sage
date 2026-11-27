package config

import (
	"strings"
	"testing"
)

// playbooks-08: in a fleet every database evaluates an objective with no
// database, so an unbound app SLO would page (and open an slo_burn
// investigation) once per database. Fleet and meta-db configs must bind
// the objective to a database or name the owner_database whose
// investigator handles its burns. Standalone keeps "empty = every
// database" (there is only one).
// No concurrent access tests: validation is a pure function of the config.

const fleetSLOPrefix = "mode: fleet\ndatabases:\n" +
	"  - {name: orders, host: db1, database: orders, user: u, password: p}\n" +
	"  - {name: billing, host: db2, database: billing, user: u, password: p}\n" +
	"sre:\n  slo:\n" + pushSection

const metaSLOPrefix = "meta_db: postgres://meta@meta.example/sage\n" +
	"sre:\n  slo:\n" + pushSection

const standaloneSLOPrefix = "mode: standalone\npostgres:\n  host: db.example\n" +
	"sre:\n  slo:\n" + pushSection

func TestSLOOwner_FleetRefusesUnboundObjective(t *testing.T) {
	_, err := loadYAMLResult(t, fleetSLOPrefix+objective("name: payments\nsource: push\n"+
		"target: 0.995"))
	if err == nil {
		t.Fatal("fleet mode accepted an app SLO with no database and no owner_database")
	}
	msg := err.Error()
	for _, want := range []string{"sre.slo.objectives[0] (payments)", "database",
		"owner_database"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not name %q", msg, want)
		}
	}
	if !strings.Contains(msg, "set database") || !strings.Contains(msg, "set owner_database") {
		t.Errorf("error %q does not name both fixes", msg)
	}
}

func TestSLOOwner_MetaDBRefusesUnboundObjective(t *testing.T) {
	_, err := loadYAMLResult(t, metaSLOPrefix+objective("name: payments\nsource: push\n"+
		"target: 0.995"))
	if err == nil || !strings.Contains(err.Error(), "(payments)") ||
		!strings.Contains(err.Error(), "owner_database") {
		t.Fatalf("meta-db fleet with an unbound SLO: err = %v", err)
	}
}

// The second unbound objective is the one named, not only the first.
func TestSLOOwner_FleetNamesTheOffendingObjective(t *testing.T) {
	_, err := loadYAMLResult(t, fleetSLOPrefix+objective("name: checkout\nsource: push\n"+
		"target: 0.999\ndatabase: orders")+
		"      - {name: payments, source: push, target: 0.995}\n")
	if err == nil || !strings.Contains(err.Error(), "sre.slo.objectives[1] (payments)") {
		t.Fatalf("err = %v, want it to name objectives[1] (payments)", err)
	}
}

func TestSLOOwner_StandaloneUnboundObjectivePasses(t *testing.T) {
	cfg, err := loadYAMLResult(t, standaloneSLOPrefix+objective("name: payments\n"+
		"source: push\ntarget: 0.995"))
	if err != nil {
		t.Fatalf("standalone with an unbound SLO: %v", err)
	}
	o := cfg.SRE.SLO.Objectives
	if len(o) != 1 || o[0].Name != "payments" || o[0].Database != "" ||
		o[0].OwnerDatabase != "" {
		t.Fatalf("objectives = %+v", o)
	}
}

func TestSLOOwner_FleetOneDatabaseBoundPasses(t *testing.T) {
	yaml := "mode: fleet\ndatabases:\n" +
		"  - {name: orders, host: db1, database: orders, user: u, password: p}\n" +
		"sre:\n  slo:\n" + pushSection +
		objective("name: checkout\nsource: push\ntarget: 0.999\ndatabase: orders")
	cfg, err := loadYAMLResult(t, yaml)
	if err != nil {
		t.Fatalf("fleet of one with a bound SLO: %v", err)
	}
	if o := cfg.SRE.SLO.Objectives; len(o) != 1 || o[0].Database != "orders" {
		t.Fatalf("objectives = %+v", o)
	}
}

func TestSLOOwner_FleetOwnerDatabasePasses(t *testing.T) {
	cfg, err := loadYAMLResult(t, fleetSLOPrefix+objective("name: payments\nsource: push\n"+
		"target: 0.995\nowner_database: billing"))
	if err != nil {
		t.Fatalf("fleet with owner_database: %v", err)
	}
	o := cfg.SRE.SLO.Objectives
	if len(o) != 1 || o[0].Database != "" || o[0].OwnerDatabase != "billing" {
		t.Fatalf("objectives = %+v", o)
	}
}

// A meta-db fleet discovers its databases, so any owner name loads (the
// sidecar warns at startup when none matches).
func TestSLOOwner_MetaDBOwnerDatabasePasses(t *testing.T) {
	cfg, err := loadYAMLResult(t, metaSLOPrefix+objective("name: payments\nsource: push\n"+
		"target: 0.995\nowner_database: not-yet-registered"))
	if err != nil {
		t.Fatalf("meta-db fleet with owner_database: %v", err)
	}
	if o := cfg.SRE.SLO.Objectives; len(o) != 1 || o[0].OwnerDatabase != "not-yet-registered" {
		t.Fatalf("objectives = %+v", o)
	}
}

func TestSLOOwner_InvalidOwnerDatabase(t *testing.T) {
	cases := map[string]struct{ yaml, want string }{
		"owner not configured": {fleetSLOPrefix + objective("name: payments\nsource: push\n"+
			"target: 0.995\nowner_database: inventory"), `"inventory"`},
		"owner with database": {fleetSLOPrefix + objective("name: payments\nsource: push\n"+
			"target: 0.995\ndatabase: orders\nowner_database: orders"), "owner_database"},
		"owner in standalone": {standaloneSLOPrefix + objective("name: payments\n"+
			"source: push\ntarget: 0.995\nowner_database: orders"), "owner_database"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := loadYAMLResult(t, c.yaml)
			if err == nil {
				t.Fatalf("accepted:\n%s", c.yaml)
			}
			if !strings.Contains(err.Error(), "(payments)") ||
				!strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q must name payments and %s", err, c.want)
			}
		})
	}
}

// Nil/empty: a fleet with no objectives at all is valid.
func TestSLOOwner_FleetNoObjectivesPasses(t *testing.T) {
	cfg, err := loadYAMLResult(t, fleetSLOPrefix)
	if err != nil || len(cfg.SRE.SLO.Objectives) != 0 {
		t.Fatalf("fleet without objectives: cfg=%v err=%v", cfg, err)
	}
}

// Validation runs on the Config itself too (reload and API writes), not
// only in Load.
func TestSLOOwner_ConfigValidate(t *testing.T) {
	c := DefaultConfig()
	c.Mode = "fleet"
	c.Databases = []DatabaseConfig{{Name: "orders", Host: "db1"},
		{Name: "billing", Host: "db2"}}
	c.SRE.SLO.Push.HMACSecret = secret32
	c.SRE.SLO.Objectives = []SREObjectiveConfig{{Name: "payments", Source: "push",
		Target: 0.995}}
	err := c.validate()
	if err == nil || !strings.Contains(err.Error(), "(payments)") {
		t.Fatalf("validate unbound: %v", err)
	}
	c.SRE.SLO.Objectives[0].OwnerDatabase = "orders"
	if err := c.validate(); err != nil {
		t.Fatalf("validate owned: %v", err)
	}
	c.SRE.SLO.Enabled = false
	c.SRE.SLO.Objectives[0].OwnerDatabase = ""
	if err := c.validate(); err == nil {
		t.Fatal("an unbound fleet SLO was accepted with the SLO layer off; " +
			"turning it on later would page once per database")
	}
}
