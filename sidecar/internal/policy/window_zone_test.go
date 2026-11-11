package policy

import (
	"encoding/json"
	"reflect"
	"testing"
)

// policy-04: a policy window with no zone is evaluated on the process
// clock, so startup warns about it, and the staffed bootstrap takes its
// zone from trust.maintenance_window when that names one.
//
// No concurrent access tests: these are pure functions of their input.

func TestWindowZone(t *testing.T) {
	for expression, want := range map[string]string{
		"weekdays 01:00-05:00 America/Chicago": "America/Chicago",
		"0 2 * * * @30m UTC":                   "UTC",
		"weekdays 01:00-05:00":                 "",
		"always":                               "",
		"":                                     "",
		"not a window":                         "",
	} {
		if got := WindowZone(expression); got != want {
			t.Fatalf("WindowZone(%q) = %q, want %q", expression, got, want)
		}
	}
}

func TestUnzonedWindows(t *testing.T) {
	doc := StaffedProfile()
	doc.MaintenanceWindows = []string{"weekdays 01:00-05:00", "always",
		"weekends America/Chicago", "nights"}
	got := UnzonedWindows(doc)
	// "always" is the same on every clock and needs no zone.
	if want := []string{"weekdays 01:00-05:00", "nights"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("UnzonedWindows = %q, want %q", got, want)
	}
	if got := UnzonedWindows(Document{}); len(got) != 0 {
		t.Fatalf("UnzonedWindows(empty) = %q, want none", got)
	}
}

func TestProfileDocumentTakesTheTrustZone(t *testing.T) {
	raw, err := profileDocumentIn("staffed", "America/Chicago")
	if err != nil {
		t.Fatalf("profileDocumentIn: %v", err)
	}
	doc, err := ParseDocument(raw)
	if err != nil {
		t.Fatalf("ParseDocument: %v", err)
	}
	if want := []string{"weekdays 01:00-05:00 America/Chicago"}; !reflect.DeepEqual(
		doc.MaintenanceWindows, want) {
		t.Fatalf("windows = %q, want %q", doc.MaintenanceWindows, want)
	}
	// Without a zone, and for the unattended profile, nothing changes.
	for profile, want := range map[string][]string{
		"staffed": {"weekdays 01:00-05:00"}, "unattended": {"always", "weekends"},
	} {
		raw, err := profileDocumentIn(profile, "")
		if err != nil {
			t.Fatalf("profileDocumentIn(%s): %v", profile, err)
		}
		var wire struct {
			Windows []string `json:"maintenance_windows"`
		}
		if err := json.Unmarshal(raw, &wire); err != nil || !reflect.DeepEqual(wire.Windows,
			want) {
			t.Fatalf("%s windows = %q (%v), want %q", profile, wire.Windows, err, want)
		}
	}
	raw, err = profileDocumentIn("unattended", "America/Chicago")
	if err != nil || !json.Valid(raw) {
		t.Fatalf("unattended with a zone: %v", err)
	}
	if _, err := profileDocumentIn("staffed", "Mars/Olympus"); err == nil {
		t.Fatal("an unknown zone was accepted")
	}
	if _, err := profileDocumentIn("mystery", ""); err == nil {
		t.Fatal("an unknown profile was accepted")
	}
}
