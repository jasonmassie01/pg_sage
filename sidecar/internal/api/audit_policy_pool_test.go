package api

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// policy-03: the evidence pack reads the standing policy from the control
// database, where the API writes it, not from the audited fleet database.
// audit-07: it reads the sign-ins and config changes there too, so the
// control pool is the pack's Control source (which also holds the policy).
func TestAuditEvidenceReadsThePolicyFromTheControlPool(t *testing.T) {
	control, audited := &pgxpool.Pool{}, &pgxpool.Pool{}
	src := (AuditDeps{Control: control}).evidenceSources(audited, "orders")
	if src.Control != control || src.DB != audited || src.DatabaseID != 0 {
		t.Fatalf("sources = %+v, want the control pool %p as Control", src, control)
	}
	src = (AuditDeps{}).evidenceSources(audited, "orders")
	if src.Control != nil || src.DB != audited {
		t.Fatalf("sources without a control pool = %+v, want the audited %p alone", src,
			audited)
	}
}

// audit-07: the database id selects config changes; the control pool is
// read as the audited one only when it is the same pool with no id
// (standalone, the control pack). A shared primary pool with an id still
// filters its config changes by that id.
func TestAuditEvidenceSources(t *testing.T) {
	control, audited := &pgxpool.Pool{}, &pgxpool.Pool{}
	ids := func(name string) (int, bool) { return 7, name == "orders" }
	d := AuditDeps{Control: control, DatabaseID: ids}
	if src := d.evidenceSources(audited, "orders"); src.DatabaseID != 7 ||
		src.Control != control {
		t.Fatalf("fleet sources = %+v, want Control and database id 7", src)
	}
	if src := d.evidenceSources(control, "control"); src.Control != nil ||
		src.DB != control || src.DatabaseID != 0 {
		t.Fatalf("control pack sources = %+v, want the control pool alone", src)
	}
	if src := d.evidenceSources(control, "orders"); src.Control != control ||
		src.DatabaseID != 7 {
		t.Fatalf("shared primary sources = %+v, want Control with id 7", src)
	}
	zero := AuditDeps{Control: control, DatabaseID: func(string) (int, bool) { return 0, true }}
	if src := zero.evidenceSources(control, "standalone"); src.Control != nil ||
		src.DatabaseID != 0 {
		t.Fatalf("id 0 = %+v, want unknown (standalone)", src)
	}
}

// No fleet manager or no registered instance: no id.
func TestFleetDatabaseIDWithoutManager(t *testing.T) {
	for _, name := range []string{"", "all", "control", "orders"} {
		if id, ok := fleetDatabaseID(nil)(name); ok || id != 0 {
			t.Fatalf("%q: id %d ok %v, want unknown", name, id, ok)
		}
	}
}
