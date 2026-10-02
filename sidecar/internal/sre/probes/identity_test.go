package probes

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// CHECK-07: every probe a two-sample investigation compares carries the
// server's identity (postmaster start, system identifier, timeline, role
// and version), so a restart or failover between the samples is detected
// instead of compared. A missing column is unknown, never a zero that
// could match another unknown sample by accident.

var identityStart = time.Date(2026, 9, 27, 6, 0, 0, 0, time.UTC)

func identityRow() Row {
	return Row{"server_started_at": identityStart,
		"system_identifier": "7692103199828676646", "timeline_id": int64(3),
		"in_recovery": false, "server_version_num": int64(170010)}
}

func TestIdentityOf_DecodesEveryField(t *testing.T) {
	id := IdentityOf(identityRow())
	if !id.StartedAt.Equal(identityStart) || id.SystemID != "7692103199828676646" ||
		id.TimelineID != 3 || id.Role != ServerRolePrimary || id.VersionNum != 170010 {
		t.Fatalf("identity = %+v", id)
	}
	if id.IsZero() {
		t.Fatal("a decoded identity reports itself unknown")
	}
	standby := identityRow()
	standby["in_recovery"] = true
	if got := IdentityOf(standby).Role; got != ServerRoleStandby {
		t.Fatalf("in_recovery true decoded as role %q, want standby", got)
	}
}

// Stored evidence decodes numbers as json.Number and times as RFC 3339
// strings; the identity must survive the round trip exactly.
func TestIdentityOf_DecodesStoredEvidence(t *testing.T) {
	row := Row{"server_started_at": identityStart.Format(time.RFC3339Nano),
		"system_identifier": "7692103199828676646", "timeline_id": json.Number("12"),
		"in_recovery": true, "server_version_num": json.Number("160004")}
	id := IdentityOf(row)
	if !id.StartedAt.Equal(identityStart) || id.TimelineID != 12 ||
		id.Role != ServerRoleStandby || id.VersionNum != 160004 {
		t.Fatalf("stored identity = %+v", id)
	}
}

func TestIdentityOf_MissingFieldsAreUnknown(t *testing.T) {
	id := IdentityOf(Row{})
	if !id.IsZero() || id.Role != "" || id.TimelineID != 0 || id.SystemID != "" ||
		id.VersionNum != 0 || !id.StartedAt.IsZero() {
		t.Fatalf("empty row identity = %+v, want all unknown", id)
	}
	// A null in_recovery is unknown, not "primary".
	if got := IdentityOf(Row{"in_recovery": nil}).Role; got != "" {
		t.Fatalf("null in_recovery decoded as %q", got)
	}
	// Nonsense values are unknown, never coerced.
	bad := IdentityOf(Row{"timeline_id": "three", "server_version_num": 1.5,
		"system_identifier": true, "in_recovery": "f"})
	if !bad.IsZero() {
		t.Fatalf("malformed identity = %+v, want unknown", bad)
	}
	// A system identifier read as a number keeps its exact digits.
	if got := IdentityOf(Row{"system_identifier": json.Number("7692103199828676646")}).
		SystemID; got != "7692103199828676646" {
		t.Fatalf("numeric system identifier = %q", got)
	}
	if IdentityOf(nil) != (ServerIdentity{}) {
		t.Fatal("a nil row has an identity")
	}
}

func TestDecoders_CarryTheServerIdentity(t *testing.T) {
	conn := identityRow()
	extra := Row{"in_current_database": true, "application_name": "api",
		"client_addr": "10.0.0.1", "state": "idle", "backends": int64(4)}
	for k, v := range extra {
		conn[k] = v
	}
	gs, err := ConnectionGroups(okResult(ConnectionSaturation, nil, conn))
	if err != nil || len(gs) != 1 || gs[0].Identity != IdentityOf(identityRow()) {
		t.Fatalf("connection identity = %+v (%v)", gs, err)
	}
	slot := identityRow()
	slot["slot_name"], slot["active"] = "cdc", true
	ss, err := Slots(okResult(ReplicationSlots, nil, slot))
	if err != nil || len(ss) != 1 || ss[0].Identity != IdentityOf(identityRow()) {
		t.Fatalf("slot identity = %+v (%v)", ss, err)
	}
	wal := identityRow()
	wal["wal_bytes"] = int64(1 << 30)
	w, err := WALStats(okResult(WALCheckpoint, nil, wal))
	if err != nil || w.Identity != IdentityOf(identityRow()) {
		t.Fatalf("wal identity = %+v (%v)", w, err)
	}
}

// The compared probes are versioned up and every SQL variant selects
// every identity column.
func TestCatalog_ComparedProbesSelectTheIdentity(t *testing.T) {
	want := map[ID]string{ConnectionSaturation: "v3", WALCheckpoint: "v2",
		ReplicationSlots: "v2"}
	for id, version := range want {
		spec, ok := Catalog().Spec(id)
		if !ok {
			t.Fatalf("%s missing from the catalog", id)
		}
		if spec.Version != version {
			t.Errorf("%s version = %s, want %s", id, spec.Version, version)
		}
		for _, v := range spec.Variants {
			for _, col := range []string{"server_started_at", "system_identifier",
				"timeline_id", "in_recovery", "server_version_num"} {
				if !strings.Contains(v.SQL, col) {
					t.Errorf("%s (min %d) does not select %s", id, v.MinVersion, col)
				}
			}
		}
	}
}
