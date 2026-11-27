package siem

import (
	"strings"
	"testing"
)

// No concurrent access tests here: mapping is a pure function of a record.

// A signed checkpoint and a prune reach the SIEM as their own events: the
// checkpoint names the head and key it signs, the prune what it removed.
func TestCheckpointAndPruneLinksMapping(t *testing.T) {
	cp := Record{Source: "orders", Chain: "action_log", Link: link("action_log", 10, "C")}
	cp.Link.RowID = 0
	cp.Link.State = `{"chain":"action_log","seq":9,"hash":"` + strings.Repeat("ab", 32) +
		`","alg":"ed25519","key_id":"k1","signature":"00"}`
	e := Map(cp)
	if e["activity_name"] != "Audit Checkpoint Signed" ||
		num(t, e, "severity_id") != SeverityInformational ||
		num(t, e, "class_uid") != ClassDatastoreActivity || num(t, e, "status_id") != 1 {
		t.Fatalf("checkpoint event = %+v", e)
	}
	msg, _ := e["message"].(string)
	if !strings.Contains(msg, "9") || !strings.Contains(msg, "k1") {
		t.Fatalf("checkpoint message %q does not name the head and key", msg)
	}
	proof := e["unmapped"].(map[string]any)["pg_sage"].(map[string]any)
	if proof["state"] != cp.Link.State || proof["op"] != "C" {
		t.Fatalf("checkpoint event does not carry its signed payload: %+v", proof)
	}
	pr := Record{Source: "orders", Chain: "auth_audit", Link: link("auth_audit", 11, "P")}
	pr.Link.State = `{"floor":4,"checkpoint":9,"checkpoint_link":10,"pruned":3}`
	e = Map(pr)
	msg, _ = e["message"].(string)
	if e["activity_name"] != "Audit Trail Pruned" ||
		num(t, e, "severity_id") != SeverityMedium ||
		num(t, e, "class_uid") != ClassAuthentication ||
		!strings.Contains(msg, "below 4") || !strings.Contains(msg, "checkpoint 9") ||
		!strings.Contains(msg, "3 link") {
		t.Fatalf("prune event = %+v", e)
	}
}

// Invalid input: a marker link whose payload does not parse is still
// exported (never dropped), with a message saying so.
func TestMarkerLinksWithUnreadablePayload(t *testing.T) {
	for _, op := range []string{"C", "P"} {
		r := Record{Source: "orders", Chain: "action_log", Link: link("action_log", 3, op)}
		r.Link.State = "{not json"
		e := Map(r)
		msg, _ := e["message"].(string)
		if !strings.Contains(msg, "unreadable") || num(t, e, "severity_id") < SeverityMedium {
			t.Fatalf("%s link with a bad payload: %+v", op, e)
		}
		if s := stubEvent(r); s["activity_name"] != e["activity_name"] {
			t.Fatalf("%s stub activity %v, event %v", op, s["activity_name"],
				e["activity_name"])
		}
	}
}
