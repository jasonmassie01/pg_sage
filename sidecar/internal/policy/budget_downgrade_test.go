package policy

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// Owner decision 2026-10-03 (downgrade safety): a saved policy carries the
// legacy single-limit fields with the performance budget, so a sidecar
// from before the split still reads it. Older parsers reject unknown
// fields, so the hygiene block is written only when it differs from the
// default (an older sidecar then fails closed on it).

// legacyDocumentWire is the document parser of v1.8.5 and earlier,
// field for field, with its strict decoding.
type legacyDocumentWire struct {
	AllowedChangeClasses    []ChangeClass   `json:"allowed_change_classes"`
	ApprovalRequiredClasses []ChangeClass   `json:"approval_required_classes,omitempty"`
	MaintenanceWindows      []string        `json:"maintenance_windows"`
	LockDurationCeilingMS   int64           `json:"lock_duration_ceiling_ms"`
	BlastRadius             legacyBlast     `json:"blast_radius"`
	Budgets                 json.RawMessage `json:"budgets"`
	RateLimits              legacyRates     `json:"rate_limits"`
	DeadlineOverrides       map[string]bool `json:"deadline_overrides"`
	RefusalSet              []string        `json:"refusal_set"`
	UnknownClassification   string          `json:"unknown_classification"`
	SerializeMode           string          `json:"serialize_mode"`
}

type legacyBlast struct {
	MaxRowsRewritten   int64 `json:"max_rows_rewritten"`
	MaxTablesPerWindow int64 `json:"max_tables_per_window"`
}

type legacyRates struct {
	MaxSelfInitiatedChangesPerWindow int64 `json:"max_self_initiated_changes_per_window"`
}

func parseAsOlderSidecar(raw []byte) (legacyDocumentWire, error) {
	var wire legacyDocumentWire
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	err := decoder.Decode(&wire)
	return wire, err
}

func TestSavedPolicyIsReadableByAnOlderSidecar(t *testing.T) {
	custom := UnattendedProfile()
	custom.BlastRadius.MaxTablesPerWindow = 7
	custom.RateLimits.MaxSelfInitiatedChangesPerWindow = 9
	for name, doc := range map[string]Document{
		"unattended profile":  UnattendedProfile(),
		"staffed profile":     StaffedProfile(),
		"legacy lifeos":       mustParse(t, lifeosStoredPolicy),
		"custom performance":  custom,
		"default hygiene set": withHygiene(UnattendedProfile(), DefaultHygieneBudget()),
	} {
		raw, err := MarshalDocument(doc)
		if err != nil {
			t.Fatalf("%s: MarshalDocument: %v", name, err)
		}
		old, err := parseAsOlderSidecar(raw)
		if err != nil {
			t.Fatalf("%s: an older sidecar cannot read the saved policy: %v\n%s",
				name, err, raw)
		}
		perf := doc.Budget(BudgetPerformance)
		if old.BlastRadius.MaxTablesPerWindow != perf.MaxTablesPerWindow ||
			old.RateLimits.MaxSelfInitiatedChangesPerWindow != perf.MaxChangesPerWindow ||
			old.BlastRadius.MaxRowsRewritten != doc.BlastRadius.MaxRowsRewritten {
			t.Fatalf("%s: older sidecar read %+v / %+v, want the performance budget %+v",
				name, old.BlastRadius, old.RateLimits, perf)
		}
		assertRoundTrip(t, name, doc, raw)
	}
}

// A customized hygiene budget has to be written; the current parser reads
// it back and the legacy fields still agree with the performance block.
func TestSavedCustomHygieneRoundTrips(t *testing.T) {
	doc := withHygiene(UnattendedProfile(), KindBudget{MaxTablesPerWindow: 3,
		MaxChangesPerWindow: 4})
	raw, err := MarshalDocument(doc)
	if err != nil {
		t.Fatalf("MarshalDocument: %v", err)
	}
	if !strings.Contains(string(raw), `"hygiene"`) {
		t.Fatalf("saved policy = %s, want the custom hygiene budget", raw)
	}
	if _, err := parseAsOlderSidecar(raw); err == nil {
		t.Fatal("an older sidecar accepted an unknown hygiene block; expected it to " +
			"fail closed (documented)")
	}
	assertRoundTrip(t, "custom hygiene", doc, raw)
	// Legacy fields equal to the performance block are not a conflict.
	agree := policyWithBlastRadius(`{"max_rows_rewritten":1,"max_tables_per_window":7,
		"performance":{"max_tables_per_window":7,"max_changes_per_window":9}}`,
		`{"max_self_initiated_changes_per_window":9}`)
	if got := mustParse(t, agree).Budget(BudgetPerformance); got != (KindBudget{7, 9}) {
		t.Fatalf("legacy == performance: budget = %+v, want {7 9}", got)
	}
}

func withHygiene(doc Document, hygiene KindBudget) Document {
	doc.BlastRadius.Hygiene = hygiene
	return doc
}

func assertRoundTrip(t *testing.T, name string, doc Document, raw []byte) {
	t.Helper()
	back := mustParse(t, string(raw))
	for _, kind := range []BudgetKind{BudgetPerformance, BudgetHygiene} {
		if back.Budget(kind) != doc.Budget(kind) {
			t.Fatalf("%s: %s budget %+v did not round-trip (%+v)", name, kind,
				doc.Budget(kind), back.Budget(kind))
		}
	}
	if back.BlastRadius.MaxRowsRewritten != doc.BlastRadius.MaxRowsRewritten {
		t.Fatalf("%s: max_rows_rewritten did not round-trip", name)
	}
}
