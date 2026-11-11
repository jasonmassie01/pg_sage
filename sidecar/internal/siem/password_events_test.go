package siem

import "testing"

// apiauth-03: password changes and admin resets map to OCSF Account Change
// activities 3 (Password Change) and 4 (Password Reset), not "Other".
func TestPasswordEventsMapToAccountChange(t *testing.T) {
	for ev, activity := range map[string]int{"password_changed": 3, "password_reset": 4} {
		e := Map(Record{Source: "db", Chain: "auth_audit", Link: link("auth_audit", 1, "I"),
			Row: map[string]any{"event": ev, "target_user_id": 7}})
		if num(t, e, "class_uid") != ClassAccountChange || num(t, e, "activity_id") != activity ||
			num(t, e, "status_id") != 1 {
			t.Fatalf("%s: class %v activity %v status %v", ev, e["class_uid"],
				e["activity_id"], e["status_id"])
		}
	}
}
