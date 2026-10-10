package policy

import "testing"

// Widens decides whether a policy proposal widens anything (AGENTDB-SPEC
// §6.11): an agent's widening proposal needs two people (G1-14). Anything
// not provably equal or tighter counts as widening.

func TestWidensFieldByField(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Document)
		widens bool
	}{
		{"identical", func(*Document) {}, false},
		{"add allowed class", func(d *Document) {
			d.AllowedChangeClasses = append(d.AllowedChangeClasses, ChangeSchemaChange)
		}, true},
		{"drop allowed class", func(d *Document) {
			d.AllowedChangeClasses = d.AllowedChangeClasses[1:]
		}, false},
		{"drop approval-required class", func(d *Document) {
			d.ApprovalRequiredClasses = d.ApprovalRequiredClasses[1:]
		}, true},
		{"add approval-required class", func(d *Document) {
			d.ApprovalRequiredClasses = append(d.ApprovalRequiredClasses, ChangeIndex)
		}, false},
		{"add window", func(d *Document) {
			d.MaintenanceWindows = append(d.MaintenanceWindows, "always")
		}, true},
		{"drop window", func(d *Document) { d.MaintenanceWindows = nil }, false},
		{"raise lock ceiling", func(d *Document) { d.LockDurationCeilingMS = 10000 }, true},
		{"lower lock ceiling", func(d *Document) { d.LockDurationCeilingMS = 1000 }, false},
		{"remove lock ceiling", func(d *Document) { d.LockDurationCeilingMS = 0 }, true},
		{"raise rows", func(d *Document) { d.BlastRadius.MaxRowsRewritten++ }, true},
		{"lower rows", func(d *Document) { d.BlastRadius.MaxRowsRewritten-- }, false},
		{"raise tables", func(d *Document) { d.BlastRadius.MaxTablesPerWindow++ }, true},
		{"raise hygiene tables", func(d *Document) {
			d.BlastRadius.Hygiene.MaxTablesPerWindow++
		}, true},
		{"raise hygiene changes", func(d *Document) {
			d.BlastRadius.Hygiene.MaxChangesPerWindow++
		}, true},
		{"raise rate", func(d *Document) {
			d.RateLimits.MaxSelfInitiatedChangesPerWindow++
		}, true},
		{"lower rate", func(d *Document) {
			d.RateLimits.MaxSelfInitiatedChangesPerWindow--
		}, false},
		{"uncap llm budget", func(d *Document) { d.Budgets.LLMTokensDaily = NoCapBudget() },
			true},
		{"raise llm budget", func(d *Document) {
			d.Budgets.LLMTokensDaily = NewBudgetLimit(500001)
		}, true},
		{"lower llm budget", func(d *Document) {
			d.Budgets.LLMTokensDaily = NewBudgetLimit(1)
		}, false},
		{"cap storage", func(d *Document) { d.Budgets.StorageBytes = NewBudgetLimit(5) },
			false},
		{"drop a budget", func(d *Document) { d.Budgets.LLMTokensDaily = BudgetLimit{} },
			true},
		{"enable deadline override", func(d *Document) {
			d.DeadlineOverrides = map[DeadlineKind]bool{DeadlineXID: true}
		}, true},
		{"disable deadline override", func(d *Document) { d.DeadlineOverrides = nil }, false},
		{"shrink refusal set", func(d *Document) { d.RefusalSet = d.RefusalSet[1:] }, true},
		{"grow refusal set", func(d *Document) {
			d.RefusalSet = append(d.RefusalSet, "major_upgrade_x")
		}, false},
		{"staffed to unattended", func(d *Document) { d.Profile = ProfileUnattended }, true},
		{"serialize mode change", func(d *Document) { d.SerializeMode = "queue" }, true},
		{"unknown classification change", func(d *Document) {
			d.UnknownClassification = "allow"
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := StaffedProfile()
			base.MaintenanceWindows = []string{"weekdays 01:00-05:00"}
			base.Budgets.LLMTokensDaily = NewBudgetLimit(500000)
			base.Budgets.StorageBytes = NoCapBudget()
			next := StaffedProfile()
			next.MaintenanceWindows = []string{"weekdays 01:00-05:00"}
			next.Budgets.LLMTokensDaily = NewBudgetLimit(500000)
			next.Budgets.StorageBytes = NoCapBudget()
			tc.mutate(&next)
			if got := Widens(base, next); got != tc.widens {
				t.Fatalf("Widens = %v, want %v", got, tc.widens)
			}
		})
	}
}

func TestWidensUnattendedToStaffedNarrows(t *testing.T) {
	if Widens(UnattendedProfile(), asStaffed(UnattendedProfile())) {
		t.Fatal("unattended -> staffed with the same bounds must not widen")
	}
}

// asStaffed is base under the staffed profile name.
func asStaffed(base Document) Document {
	next := base
	next.Profile = ProfileStaffed
	return next
}

func TestWidensClassOrderIrrelevant(t *testing.T) {
	base := StaffedProfile()
	next := StaffedProfile()
	classes := append([]ChangeClass(nil), next.AllowedChangeClasses...)
	for i, j := 0, len(classes)-1; i < j; i, j = i+1, j-1 {
		classes[i], classes[j] = classes[j], classes[i]
	}
	next.AllowedChangeClasses = classes
	if Widens(base, next) {
		t.Fatal("reordering classes does not widen")
	}
}

func TestWidensDocumentJSON(t *testing.T) {
	base := StaffedProfile()
	raw, err := MarshalDocument(base)
	if err != nil {
		t.Fatal(err)
	}
	widening := base
	widening.LockDurationCeilingMS = base.LockDurationCeilingMS * 10
	wideRaw, err := MarshalDocument(widening)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := WidensJSON(raw, wideRaw); err != nil || !got {
		t.Fatalf("WidensJSON = %v, %v; want true", got, err)
	}
	if got, err := WidensJSON(raw, raw); err != nil || got {
		t.Fatalf("WidensJSON(same) = %v, %v; want false", got, err)
	}
	if _, err := WidensJSON(raw, []byte(`{"nope":1}`)); err == nil {
		t.Fatal("an invalid document must be an error")
	}
	if got, err := WidensJSON([]byte(`garbage`), raw); err == nil || !got {
		t.Fatalf("an unreadable base = %v, %v; want an error that fails widening-closed",
			got, err)
	}
}
