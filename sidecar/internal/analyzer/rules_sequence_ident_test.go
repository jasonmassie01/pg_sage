package analyzer

import "testing"

// playbooks-03b: the sequence_exhaustion finding names its sequence as the
// catalog spells it (schema.name, unquoted). The runway finding for the
// same sequence quotes each part with quote_ident, so the cases API
// matches both spellings (internal/api/cases_sequence_runway.go). This
// pins the analyzer's half; the runway's half is pinned in
// internal/runway/sequence_ident_db_test.go.
func TestSequenceExhaustion_IdentifierIsCatalogSpelling(t *testing.T) {
	for _, name := range []string{"orders_id_seq", "Orders_Seq", "user"} {
		f := onlySequenceFinding(t, ruleSequenceExhaustion(
			seqSnap(ascSeq(name, 91)), nil, nil, nil))
		if want := "public." + name; f.ObjectIdentifier != want {
			t.Fatalf("identifier = %q, want %q", f.ObjectIdentifier, want)
		}
	}
}
