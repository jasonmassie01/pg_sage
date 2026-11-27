package earned

import (
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/policy"
)

// ux-06b (decision D6): one autonomy scale per class. ModeOf turns the
// inputs the gate reads into one of four sentences, with the first
// limiting input first in Because. Every expected mode below is written
// by hand; autonomy_mode_gate_test.go checks each against the gate.

var modeEpoch = time.Date(2026, 11, 18, 3, 0, 0, 0, time.UTC)

// modeContracts mirrors the executor's contracts for the classes the
// tests use (hand-written, so a contract change cannot hide here).
func modeContracts() map[ActionClass]ClassContract {
	return map[ActionClass]ClassContract{
		ClassVacuum: {RiskTier: policy.RiskSafe, Rollback: policy.RollbackNoRollbackNeeded,
			ChangeClass: policy.ChangeVacuum},
		ClassFreeze: {RiskTier: policy.RiskSafe, Rollback: policy.RollbackNoRollbackNeeded,
			ChangeClass: policy.ChangeFreeze},
		ClassIndexCreate: {RiskTier: policy.RiskModerate, Rollback: policy.RollbackReversible,
			ChangeClass: policy.ChangeIndex},
		ClassReindex: {RiskTier: policy.RiskModerate, Rollback: policy.RollbackReversible,
			ApprovalRequired: true, ChangeClass: policy.ChangeIndex},
		ClassRetention: {RiskTier: policy.RiskModerate, Rollback: policy.RollbackNotReversible,
			ChangeClass: policy.ChangeRetention},
		ClassSchemaChange: {RiskTier: policy.RiskHigh, Rollback: policy.RollbackForwardFixOnly,
			ApprovalRequired: true, ChangeClass: policy.ChangeSchemaChange},
		ClassBackendCancel: {RiskTier: policy.RiskModerate, Rollback: "mitigation_only",
			ApprovalRequired: true, ChangeClass: policy.ChangeBackendSignal},
	}
}

// modeCeiling is a live, attached executor with an old ramp and a
// configured window, the ledger enforced.
func modeCeiling(trust, mode string, safe, moderate bool) Ceiling {
	return Ceiling{GateAttached: true, ExecutorEnabled: true, TrustLevel: trust,
		ExecutionMode: mode, Tier3Safe: safe, Tier3Moderate: moderate,
		WindowConfigured: true, LedgerEnforced: true,
		RampStart: modeEpoch.Add(-60 * 24 * time.Hour), Now: modeEpoch,
		Contracts: modeContracts()}
}

func fullCeiling() Ceiling {
	return modeCeiling(policy.TrustAutonomous, policy.ExecutionAuto, true, true)
}

func modeRow(kind string, class ActionClass, level Level) TrustRow {
	family := FamilyTuning
	if kind == KindIncident {
		family = FamilyWraparound
	}
	spec, _ := Spec(class)
	effective := level
	return TrustRow{Family: family, Kind: kind, Class: class, Level: level,
		Effective: &effective, Cap: L3, Reversibility: spec.Reversibility}
}

func selfRow(class ActionClass, level Level) TrustRow {
	return modeRow(KindSelfInitiated, class, level)
}

type modeCase struct {
	name    string
	row     TrustRow
	ceil    Ceiling
	want    ModeName
	because string // a substring of Because[0]
}

func with(c Ceiling, edit func(*Ceiling)) Ceiling {
	edit(&c)
	return c
}

func handModeCases() []modeCase {
	return append(settingModeCases(), ledgerModeCases()...)
}

// settingModeCases are the hand-written rows the operator's settings decide.
func settingModeCases() []modeCase {
	advisorySafe := modeCeiling(policy.TrustAdvisory, policy.ExecutionAuto, true, false)
	return []modeCase{
		{"autonomous moderate at L3", selfRow(ClassIndexCreate, L3), fullCeiling(),
			ModeActsInWindow, "maintenance window"},
		{"autonomous safe at L3", selfRow(ClassVacuum, L3), fullCeiling(),
			ModeActsThenTells, "earned level L3"},
		{"advisory safe at L3", selfRow(ClassVacuum, L3), advisorySafe,
			ModeActsThenTells, "earned level L3"},
		{"advisory moderate at L3", selfRow(ClassIndexCreate, L3), advisorySafe,
			ModeAsksFirst, "trust ceiling advisory"},
		{"approval mode", selfRow(ClassVacuum, L3),
			modeCeiling(policy.TrustAdvisory, policy.ExecutionApproval, true, true),
			ModeAsksFirst, "execution mode is approval"},
		{"observation", selfRow(ClassVacuum, L3),
			modeCeiling(policy.TrustObservation, policy.ExecutionAuto, true, true),
			ModeObserves, "trust ceiling observation"},
		{"manual", selfRow(ClassVacuum, L3),
			modeCeiling(policy.TrustAutonomous, policy.ExecutionManual, true, true),
			ModeObserves, "execution mode is manual"},
		{"tier3_moderate off", selfRow(ClassIndexCreate, L3),
			modeCeiling(policy.TrustAutonomous, policy.ExecutionAuto, true, false),
			ModeObserves, "tier3_moderate is off"},
		{"tier3_safe off", selfRow(ClassVacuum, L3),
			modeCeiling(policy.TrustAdvisory, policy.ExecutionAuto, false, false),
			ModeObserves, "tier3_safe is off"},
	}
}

// ledgerModeCases are the hand-written rows the ledger and the stops decide.
func ledgerModeCases() []modeCase {
	advisorySafe := modeCeiling(policy.TrustAdvisory, policy.ExecutionAuto, true, false)
	downgraded := selfRow(ClassIndexCreate, L3)
	l1 := L1
	downgraded.Effective, downgraded.Downgrades = &l1, []Downgrade{{Reason: "error_budget"}}
	return []modeCase{
		{"earned L2", selfRow(ClassIndexCreate, L2), fullCeiling(), ModeAsksFirst,
			"earned level L2"},
		{"earned L1", selfRow(ClassIndexCreate, L1), fullCeiling(), ModeObserves,
			"earned level L1"},
		{"earned L0", selfRow(ClassIndexCreate, L0), fullCeiling(), ModeObserves,
			"earned level L0"},
		{"irreversible at L3", selfRow(ClassRetention, L3), fullCeiling(), ModeObserves,
			"irreversible"},
		{"approval-required class", selfRow(ClassReindex, L3), fullCeiling(),
			ModeAsksFirst, "always needs approval"},
		{"high risk forward-fix", selfRow(ClassSchemaChange, L3), fullCeiling(),
			ModeObserves, "irreversible"},
		{"mitigation-only cancel", selfRow(ClassBackendCancel, L3), fullCeiling(),
			ModeAsksFirst, "always needs approval"},
		{"incident safe at L3", modeRow(KindIncident, ClassVacuum, L3), fullCeiling(),
			ModeActsInWindow, "maintenance window"},
		{"ledger off", selfRow(ClassIndexCreate, L0),
			with(fullCeiling(), func(c *Ceiling) { c.LedgerEnforced = false }),
			ModeActsInWindow, "not enforced"},
		{"ledger off, ramp running", selfRow(ClassVacuum, L3),
			with(fullCeiling(), func(c *Ceiling) {
				c.LedgerEnforced, c.RampStart = false, modeEpoch.Add(-time.Hour)
			}), ModeObserves, "trust ramp"},
		{"incident, ramp running", modeRow(KindIncident, ClassVacuum, L3),
			with(fullCeiling(), func(c *Ceiling) { c.RampStart = modeEpoch.Add(-time.Hour) }),
			ModeObserves, "trust ramp"},
		{"degraded validation", selfRow(ClassVacuum, L3),
			with(advisorySafe, func(c *Ceiling) { c.SQLValidationDegraded = true }),
			ModeAsksFirst, "SQL validation is degraded"},
		{"no window configured", selfRow(ClassIndexCreate, L3),
			with(fullCeiling(), func(c *Ceiling) { c.WindowConfigured = false }),
			ModeObserves, "no maintenance window"},
		{"downgraded", downgraded, fullCeiling(), ModeObserves, "downgraded: error_budget"},
	}
}

func TestModeOf_HandWrittenTable(t *testing.T) {
	for _, tc := range handModeCases() {
		t.Run(tc.name, func(t *testing.T) {
			got := ModeOf(tc.row, tc.ceil)
			if got.Name != tc.want {
				t.Fatalf("mode = %s %v, want %s", got.Name, got.Because, tc.want)
			}
			if len(got.Because) == 0 || !strings.Contains(got.Because[0], tc.because) {
				t.Fatalf("because = %v, want first item containing %q", got.Because,
					tc.because)
			}
		})
	}
}

func TestModeOf_SentencesAreTheScale(t *testing.T) {
	want := map[ModeName]string{ModeObserves: "watches only", ModeAsksFirst: "asks first",
		ModeActsThenTells: "acts, then tells you", ModeActsInWindow: "acts in the window"}
	for mode, sentence := range want {
		if got := mode.Sentence(); got != sentence {
			t.Errorf("%s.Sentence() = %q, want %q", mode, got, sentence)
		}
	}
	if got := ModeName("bogus").Sentence(); got != "watches only" {
		t.Errorf("unknown mode sentence = %q, want the fail-closed watches only", got)
	}
}

func TestModeOf_InvalidInputFailsClosed(t *testing.T) {
	unknownTier := fullCeiling()
	unknownTier.Contracts = map[ActionClass]ClassContract{ClassVacuum: {RiskTier: "bogus",
		Rollback: policy.RollbackReversible, ChangeClass: policy.ChangeVacuum}}
	cases := []modeCase{
		{"unknown trust level", selfRow(ClassVacuum, L3),
			modeCeiling("yolo", policy.ExecutionAuto, true, true), ModeObserves,
			`unknown trust level "yolo"`},
		{"empty trust level", selfRow(ClassVacuum, L3),
			modeCeiling("", policy.ExecutionAuto, true, true), ModeObserves,
			"unknown trust level"},
		{"unknown execution mode", selfRow(ClassVacuum, L3),
			modeCeiling(policy.TrustAutonomous, "yolo", true, true), ModeObserves,
			`unknown execution mode "yolo"`},
		{"unknown risk tier", selfRow(ClassVacuum, L3), unknownTier, ModeObserves,
			"unknown risk tier"},
		{"class without a contract", selfRow(ClassSlotDrop, L3), fullCeiling(),
			ModeObserves, "no action contract"},
		{"unknown class", selfRow("bogus_class", L3), fullCeiling(), ModeObserves,
			"no action contract"},
	}
	for _, tc := range cases {
		got := ModeOf(tc.row, tc.ceil)
		if got.Name != ModeObserves || len(got.Because) == 0 ||
			!strings.Contains(got.Because[0], tc.because) {
			t.Errorf("%s: mode = %s %v, want observes because %q", tc.name, got.Name,
				got.Because, tc.because)
		}
	}
}

func TestModeOf_ZeroCeilingAndNilEffective(t *testing.T) {
	got := ModeOf(selfRow(ClassVacuum, L3), Ceiling{})
	if got.Name != ModeObserves || len(got.Because) != 1 ||
		!strings.Contains(got.Because[0], "no executor") {
		t.Fatalf("zero ceiling = %s %v, want observes: no executor", got.Name, got.Because)
	}
	row := selfRow(ClassIndexCreate, L3)
	row.Effective, row.Cap = nil, L2
	if got := ModeOf(row, fullCeiling()); got.Name != ModeAsksFirst ||
		!strings.Contains(got.Because[0], "earned level L2") {
		t.Fatalf("nil effective, cap L2 = %s %v, want asks first at L2", got.Name, got.Because)
	}
	row.Cap = L3
	if got := ModeOf(row, fullCeiling()); got.Name != ModeActsInWindow {
		t.Fatalf("nil effective, cap L3 = %s %v, want acts in the window", got.Name,
			got.Because)
	}
	if got := ModeOf(TrustRow{}, fullCeiling()); got.Name != ModeObserves {
		t.Fatalf("zero row = %s %v, want observes", got.Name, got.Because)
	}
}

func everyCeiling() []Ceiling {
	var out []Ceiling
	for _, trust := range []string{policy.TrustObservation, policy.TrustAdvisory,
		policy.TrustAutonomous} {
		for _, mode := range []string{policy.ExecutionManual, policy.ExecutionApproval,
			policy.ExecutionAuto} {
			for _, flags := range [][2]bool{{false, false}, {true, false}, {true, true}} {
				for _, enforced := range []bool{true, false} {
					c := modeCeiling(trust, mode, flags[0], flags[1])
					c.LedgerEnforced = enforced
					out = append(out, c)
				}
			}
		}
	}
	return out
}

func TestModeOf_IrreversibleNeverAboveAsksFirstWhenEnforced(t *testing.T) {
	for _, ceil := range everyCeiling() {
		if !ceil.LedgerEnforced {
			continue // without the ledger the gate's tiers decide: see the gate test
		}
		for _, class := range []ActionClass{ClassRetention, ClassSchemaChange} {
			for level := L0; level <= L3; level++ {
				for _, kind := range []string{KindSelfInitiated, KindIncident} {
					got := ModeOf(modeRow(kind, class, level), ceil)
					if got.Name == ModeActsThenTells || got.Name == ModeActsInWindow {
						t.Fatalf("%s %s at %s under %+v = %s", kind, class, level, ceil,
							got.Name)
					}
				}
			}
		}
	}
}

func TestModeOf_StopsForceObserves(t *testing.T) {
	stops := map[string]func(*Ceiling){
		"emergency stop":    func(c *Ceiling) { c.EmergencyStop = true },
		"replica":           func(c *Ceiling) { c.Replica = true },
		"executor disabled": func(c *Ceiling) { c.ExecutorEnabled = false },
	}
	for name, stop := range stops {
		for class := range modeContracts() {
			for level := L0; level <= L3; level++ {
				got := ModeOf(selfRow(class, level), with(fullCeiling(), stop))
				if got.Name != ModeObserves || len(got.Because) == 0 ||
					!strings.Contains(got.Because[0], strings.Split(name, " ")[0]) {
					t.Fatalf("%s: %s at %s = %s %v", name, class, level, got.Name,
						got.Because)
				}
			}
		}
	}
}

func TestModeOf_LevelTransitionsFollowTheCeiling(t *testing.T) {
	steps := []struct {
		level         Level
		full, advised ModeName
	}{
		{L1, ModeObserves, ModeObserves},
		{L2, ModeAsksFirst, ModeAsksFirst},
		{L3, ModeActsInWindow, ModeAsksFirst},
	}
	advisory := modeCeiling(policy.TrustAdvisory, policy.ExecutionAuto, true, true)
	for _, s := range steps {
		row := selfRow(ClassIndexCreate, s.level)
		if got := ModeOf(row, fullCeiling()).Name; got != s.full {
			t.Errorf("%s under autonomous = %s, want %s", s.level, got, s.full)
		}
		if got := ModeOf(row, advisory).Name; got != s.advised {
			t.Errorf("%s under advisory = %s, want %s", s.level, got, s.advised)
		}
	}
}

func TestModeOf_PolicySetCeilingOnlyLowers(t *testing.T) {
	ceil := fullCeiling()
	ceil.ClassTrust = map[policy.ChangeClass]string{policy.ChangeIndex: policy.TrustAdvisory}
	got := ModeOf(selfRow(ClassIndexCreate, L3), ceil)
	if got.Name != ModeAsksFirst || !strings.Contains(got.Because[0], "policy set") {
		t.Fatalf("set ceiling advisory = %s %v, want asks first because policy set",
			got.Name, got.Because)
	}
	if got := ModeOf(selfRow(ClassVacuum, L3), ceil); got.Name != ModeActsThenTells {
		t.Fatalf("another class = %s %v, want unchanged acts then tells", got.Name,
			got.Because)
	}
	low := modeCeiling(policy.TrustAdvisory, policy.ExecutionAuto, true, true)
	low.ClassTrust = map[policy.ChangeClass]string{policy.ChangeIndex: policy.TrustAutonomous}
	if got := ModeOf(selfRow(ClassIndexCreate, L3), low); got.Name != ModeAsksFirst ||
		!strings.Contains(got.Because[0], "trust ceiling advisory") {
		t.Fatalf("a higher set ceiling raised the mode: %s %v", got.Name, got.Because)
	}
	low.ClassTrust[policy.ChangeIndex] = "bogus"
	if got := ModeOf(selfRow(ClassIndexCreate, L3), low); got.Name != ModeObserves {
		t.Fatalf("unknown set ceiling = %s %v, want observes", got.Name, got.Because)
	}
}

func TestAnnotateMode_FillsEveryRow(t *testing.T) {
	v := TrustView{Rows: []TrustRow{selfRow(ClassVacuum, L3), selfRow(ClassIndexCreate, L1)}}
	AnnotateMode(&v, fullCeiling())
	if v.Rows[0].Mode != ModeActsThenTells || v.Rows[1].Mode != ModeObserves ||
		len(v.Rows[1].ModeBecause) == 0 || v.Rows[1].ModeBecause[0] != "earned level L1" {
		t.Fatalf("rows = %+v", v.Rows)
	}
	AnnotateMode(nil, fullCeiling()) // a nil view is a no-op, not a panic
	empty := TrustView{}
	AnnotateMode(&empty, fullCeiling())
	if len(empty.Rows) != 0 {
		t.Fatalf("empty view gained rows: %+v", empty.Rows)
	}
}

// No concurrent access test here: ModeOf is a pure function of its two
// value arguments; AnnotateMode writes only the view it is given.
