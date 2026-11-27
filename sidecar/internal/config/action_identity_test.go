package config

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

const observationURL = "postgres://sage_observer:pw@db.internal:5432/orders?sslmode=require"

// Happy path: the action identity reaches the same server and database
// under its own role.
func TestValidateActionDSN_SameTargetOtherRole(t *testing.T) {
	err := ValidateActionDSN(observationURL,
		"postgres://sage_actor:pw2@db.internal:5432/orders?sslmode=require")
	if err != nil {
		t.Fatalf("ValidateActionDSN = %v, want nil", err)
	}
}

// Key/value DSNs are compared by what they connect to, not by spelling.
func TestValidateActionDSN_KeyValueForm(t *testing.T) {
	err := ValidateActionDSN(observationURL,
		"host=db.internal port=5432 dbname=orders user=sage_actor password=x")
	if err != nil {
		t.Fatalf("ValidateActionDSN(key/value) = %v, want nil", err)
	}
}

// Invalid input: another host, port or database is refused at startup,
// each naming what differs.
func TestValidateActionDSN_RefusesAnotherTarget(t *testing.T) {
	cases := map[string]struct{ dsn, want string }{
		"host": {"postgres://sage_actor:pw@other.internal:5432/orders", "host"},
		"port": {"postgres://sage_actor:pw@db.internal:5433/orders", "port"},
		"database": {"postgres://sage_actor:pw@db.internal:5432/billing",
			"database"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			err := ValidateActionDSN(observationURL, c.dsn)
			if !errors.Is(err, ErrActionDSNTarget) {
				t.Fatalf("ValidateActionDSN = %v, want ErrActionDSNTarget", err)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q does not name the %s", err, c.want)
			}
			if strings.Contains(err.Error(), "pw") {
				t.Fatalf("error %q leaks the password", err)
			}
		})
	}
}

// Boundary: the same role on both connections is not a separation.
func TestValidateActionDSN_RefusesTheObservationRole(t *testing.T) {
	err := ValidateActionDSN(observationURL,
		"postgres://sage_observer:pw@db.internal:5432/orders")
	if !errors.Is(err, ErrActionDSNTarget) || !strings.Contains(err.Error(), "role") {
		t.Fatalf("ValidateActionDSN(same role) = %v, want a refusal naming the role", err)
	}
}

// Invalid input: an unparsable action DSN is refused, without echoing it.
func TestValidateActionDSN_RefusesGarbage(t *testing.T) {
	err := ValidateActionDSN(observationURL, "postgres://sage_actor:pw@[bad")
	if err == nil {
		t.Fatal("ValidateActionDSN(garbage) = nil, want an error")
	}
	if strings.Contains(err.Error(), "pw@") {
		t.Fatalf("error %q leaks the DSN", err)
	}
}

// Nil/empty: no action_dsn is the single-identity default and always valid.
func TestValidateActionDSN_EmptyIsSingleIdentity(t *testing.T) {
	if err := ValidateActionDSN(observationURL, ""); err != nil {
		t.Fatalf("ValidateActionDSN(\"\") = %v, want nil", err)
	}
	if err := ValidateActionDSN("", ""); err != nil {
		t.Fatalf("ValidateActionDSN(\"\", \"\") = %v, want nil", err)
	}
}

// Integration with Load: a fleet database whose action_dsn reaches another
// database is refused when the config loads.
func TestLoad_FleetActionDSNOnAnotherDatabaseIsRefused(t *testing.T) {
	_, err := loadYAMLResult(t, `
mode: fleet
databases:
  - name: orders
    host: db.internal
    port: 5432
    user: sage_observer
    password: pw
    database: orders
    action_dsn: postgres://sage_actor:pw2@db.internal:5432/billing
`)
	if !errors.Is(err, ErrActionDSNTarget) {
		t.Fatalf("Load = %v, want ErrActionDSNTarget", err)
	}
	if !strings.Contains(err.Error(), "databases[0]") ||
		!strings.Contains(err.Error(), "orders") {
		t.Fatalf("error %q does not name the database entry", err)
	}
}

func TestLoad_FleetActionDSNIsKept(t *testing.T) {
	cfg, err := loadYAMLResult(t, `
mode: fleet
databases:
  - name: orders
    host: db.internal
    port: 5432
    user: sage_observer
    password: pw
    database: orders
    action_dsn: postgres://sage_actor:pw2@db.internal:5432/orders
  - name: users
    host: users.internal
    port: 5432
    user: sage_observer
    password: pw
    database: users
`)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Databases[0].ActionDSN; got !=
		"postgres://sage_actor:pw2@db.internal:5432/orders" {
		t.Fatalf("databases[0].action_dsn = %q", got)
	}
	if got := cfg.Databases[1].ActionDSN; got != "" {
		t.Fatalf("databases[1].action_dsn = %q, want empty (single identity)", got)
	}
}

// Standalone: postgres.action_dsn is checked against the postgres
// connection and carried onto the synthesized database.
func TestLoad_StandaloneActionDSN(t *testing.T) {
	cfg, err := loadYAMLResult(t, `
mode: standalone
postgres:
  host: localhost
  port: 5432
  user: sage_observer
  database: app
  action_dsn: postgres://sage_actor:x@localhost:5432/app
`)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Postgres.ActionDSN == "" {
		t.Fatal("postgres.action_dsn was dropped")
	}
	if len(cfg.Databases) != 1 || cfg.Databases[0].ActionDSN != cfg.Postgres.ActionDSN {
		t.Fatalf("synthesized database action_dsn = %+v", cfg.Databases)
	}
	_, err = loadYAMLResult(t, `
mode: standalone
postgres:
  host: localhost
  port: 5432
  user: sage_observer
  database: app
  action_dsn: postgres://sage_actor:x@elsewhere:5432/app
`)
	if !errors.Is(err, ErrActionDSNTarget) || !strings.Contains(err.Error(), "postgres") {
		t.Fatalf("Load(standalone, other host) = %v, want ErrActionDSNTarget", err)
	}
}

// The action DSN carries a password: it is a secret field.
func TestActionDSNIsSecret(t *testing.T) {
	for _, typ := range []reflect.Type{reflect.TypeOf(DatabaseConfig{}),
		reflect.TypeOf(PostgresConfig{})} {
		field, ok := typ.FieldByName("ActionDSN")
		if !ok || field.Tag.Get("secret") != "true" ||
			field.Tag.Get("yaml") != "action_dsn" {
			t.Fatalf("%s.ActionDSN must be the secret action_dsn field: %v", typ.Name(),
				field.Tag)
		}
	}
}

// State transitions: adding or removing action_dsn on reload rebuilds the
// database's runtime, so it falls back to (or leaves) the single role.
func TestClassifyDatabaseChange_ActionDSNRebuilds(t *testing.T) {
	split := DatabaseConfig{Name: "orders", Host: "db", Port: 5432, User: "obs",
		Database: "orders", ActionDSN: "postgres://act@db:5432/orders"}
	single := split
	single.ActionDSN = ""
	for name, c := range map[string][2]DatabaseConfig{
		"removed": {split, single}, "added": {single, split}} {
		change := ClassifyDatabaseChange(c[0], c[1])
		if !change.Rebuild || len(change.Fields) != 1 || change.Fields[0] != "action_dsn" {
			t.Fatalf("%s: change = %+v, want a rebuild of action_dsn", name, change)
		}
	}
	if change := ClassifyDatabaseChange(split, split); change.Changed() {
		t.Fatalf("unchanged action_dsn reported %+v", change)
	}
}
