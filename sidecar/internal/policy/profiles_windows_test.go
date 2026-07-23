package policy

import (
	"strings"
	"testing"
	"time"
)

func TestShippedProfilesAreValidAndSafetyDistinct(t *testing.T) {
	staffed := StaffedProfile()
	unattended := UnattendedProfile()

	if err := ValidateDocument(staffed); err != nil {
		t.Fatalf("staffed profile invalid: %v", err)
	}
	if err := ValidateDocument(unattended); err != nil {
		t.Fatalf("unattended profile invalid: %v", err)
	}
	if staffed.Profile != ProfileStaffed {
		t.Fatalf("staffed.Profile = %q", staffed.Profile)
	}
	if unattended.Profile != ProfileUnattended {
		t.Fatalf("unattended.Profile = %q", unattended.Profile)
	}
	if staffed.DeadlineOverrides[DeadlineXID] ||
		staffed.DeadlineOverrides[DeadlineDisk] {
		t.Fatalf("staffed deadline overrides = %#v, want both false",
			staffed.DeadlineOverrides)
	}
	if !unattended.DeadlineOverrides[DeadlineXID] ||
		!unattended.DeadlineOverrides[DeadlineDisk] {
		t.Fatalf("unattended deadline overrides = %#v, want both true",
			unattended.DeadlineOverrides)
	}
	if !requiresApproval(staffed, ChangeOnlineMigration) {
		t.Fatal("staffed online migration must require approval")
	}
	if !requiresApproval(unattended, ChangeOnlineMigration) {
		t.Fatal("unattended structural change must remain recommend-only")
	}
	if len(unattended.MaintenanceWindows) <= len(staffed.MaintenanceWindows) {
		t.Fatalf("unattended windows = %#v, staffed = %#v; want wider shipped policy",
			unattended.MaintenanceWindows, staffed.MaintenanceWindows)
	}
}

func TestProfileValuesAreIndependentCopies(t *testing.T) {
	first := StaffedProfile()
	second := StaffedProfile()
	first.AllowedChangeClasses[0] = "mutated"
	first.DeadlineOverrides[DeadlineDisk] = true

	if second.AllowedChangeClasses[0] == "mutated" {
		t.Fatal("StaffedProfile shares AllowedChangeClasses backing array")
	}
	if second.DeadlineOverrides[DeadlineDisk] {
		t.Fatal("StaffedProfile shares DeadlineOverrides map")
	}
}

func TestParseWindowHonorsSundayDayOfWeek(t *testing.T) {
	window, err := ParseWindow("0 2 * * 0")
	if err != nil {
		t.Fatalf("ParseWindow: %v", err)
	}
	sunday := time.Date(2026, 7, 26, 2, 0, 0, 0, time.UTC)
	monday := time.Date(2026, 7, 27, 2, 0, 0, 0, time.UTC)

	if !window.Contains(sunday) {
		t.Fatal("Sunday 02:00 not in Sunday-only cron window")
	}
	if window.Contains(monday) {
		t.Fatal("Monday 02:00 unexpectedly in Sunday-only cron window")
	}
}

func TestParseWindowHonorsDayOfMonth(t *testing.T) {
	window, err := ParseWindow("0 2 15 * *")
	if err != nil {
		t.Fatalf("ParseWindow: %v", err)
	}
	fifteenth := time.Date(2026, 7, 15, 2, 0, 0, 0, time.UTC)
	sixteenth := time.Date(2026, 7, 16, 2, 0, 0, 0, time.UTC)

	if !window.Contains(fifteenth) || window.Contains(sixteenth) {
		t.Fatalf("DOM match: fifteenth=%v sixteenth=%v",
			window.Contains(fifteenth), window.Contains(sixteenth))
	}
}

func TestParseWindowUsesCronDOMDOWORSemantics(t *testing.T) {
	window, err := ParseWindow("0 2 15 * 0")
	if err != nil {
		t.Fatalf("ParseWindow: %v", err)
	}
	sundayNot15th := time.Date(2026, 7, 26, 2, 0, 0, 0, time.UTC)
	fifteenthNotSunday := time.Date(2026, 7, 15, 2, 0, 0, 0, time.UTC)
	neither := time.Date(2026, 7, 16, 2, 0, 0, 0, time.UTC)

	if !window.Contains(sundayNot15th) || !window.Contains(fifteenthNotSunday) {
		t.Fatal("cron DOM/DOW fields must use standard OR semantics")
	}
	if window.Contains(neither) {
		t.Fatal("date matching neither DOM nor DOW unexpectedly allowed")
	}
}

func TestFriendlyWindowAndOvernightRange(t *testing.T) {
	window, err := ParseWindow("weekdays 22:00-04:00")
	if err != nil {
		t.Fatalf("ParseWindow: %v", err)
	}
	mondayLate := time.Date(2026, 7, 27, 23, 0, 0, 0, time.UTC)
	tuesdayEarly := time.Date(2026, 7, 28, 3, 0, 0, 0, time.UTC)
	saturdayLate := time.Date(2026, 7, 25, 23, 0, 0, 0, time.UTC)

	if !window.Contains(mondayLate) || !window.Contains(tuesdayEarly) {
		t.Fatal("weekday overnight range did not include both sides of midnight")
	}
	if window.Contains(saturdayLate) {
		t.Fatal("weekend unexpectedly matched weekday window")
	}
}

func TestParseWindowRejectsInvalidExpressions(t *testing.T) {
	for _, expression := range []string{
		"", "not-a-window", "61 2 * * *", "0 25 * * *", "weekdays 25:00-26:00",
	} {
		t.Run(expression, func(t *testing.T) {
			_, err := ParseWindow(expression)
			if err == nil || !strings.Contains(err.Error(), "window") {
				t.Fatalf("ParseWindow(%q) error = %v", expression, err)
			}
		})
	}
}

func requiresApproval(doc Document, class ChangeClass) bool {
	for _, item := range doc.ApprovalRequiredClasses {
		if item == class {
			return true
		}
	}
	return false
}
