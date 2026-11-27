package selfmonitor

import (
	"maps"
	"testing"
)

// No concurrent access tests: IsFinding is a pure function of its input.

// An approval reason is pg_sage's explanation to the operator and may name
// pg_sage; it does not make the finding about pg_sage (advisors-07: every
// idle_in_transaction_session_timeout proposal was dropped as a
// self-monitoring finding because its reason says "pg_sage cannot verify").
func TestIsFindingIgnoresTheApprovalReason(t *testing.T) {
	detail := map[string]any{
		"approval_required": "it ends sessions, which pg_sage cannot verify is harmless",
		"rationale":         "bound idle-in-transaction sessions",
	}
	before := maps.Clone(detail)
	f := FindingFields{Category: "connection_tuning", ObjectType: "configuration",
		ObjectIdentifier: "instance", Detail: detail,
		RecommendedSQL: `ALTER DATABASE "orders" SET idle_in_transaction_session_timeout = '5min'`}
	if IsFinding(f) {
		t.Fatal("a finding whose only pg_sage mention is its approval reason was dropped")
	}
	if !maps.Equal(before, detail) {
		t.Fatalf("IsFinding changed the detail: %v, want %v", detail, before)
	}
}

// Every other detail key is still scanned: pg_sage in the rationale, or a
// pg_sage statement next to an approval reason, still marks it pg_sage's.
func TestIsFindingStillScansTheRestOfTheDetail(t *testing.T) {
	for name, detail := range map[string]map[string]any{
		"rationale names pg_sage": {"approval_required": "operator approval",
			"rationale": "pg_sage's own polling holds these sessions"},
		"statement of pg_sage": {"approval_required": "operator approval",
			"query": "SELECT 1 /* pg_sage */"},
		"nested pg_sage text": {"model": map[string]any{"note": "from pg_sage"}},
	} {
		t.Run(name, func(t *testing.T) {
			f := FindingFields{Category: "connection_tuning", ObjectIdentifier: "instance",
				Detail: detail}
			if !IsFinding(f) {
				t.Fatalf("detail %v is pg_sage's, want IsFinding true", detail)
			}
		})
	}
}

// Nil and empty details, and a detail holding only the explanation, are
// not pg_sage's.
func TestIsFindingExplanationOnlyAndEmptyDetails(t *testing.T) {
	for name, detail := range map[string]map[string]any{
		"nil":              nil,
		"empty":            {},
		"explanation only": {"approval_required": "pg_sage waits for an operator"},
	} {
		t.Run(name, func(t *testing.T) {
			f := FindingFields{Category: "wal_tuning", ObjectIdentifier: "instance",
				Detail: detail}
			if IsFinding(f) {
				t.Fatalf("detail %v marked as pg_sage's", detail)
			}
		})
	}
}
