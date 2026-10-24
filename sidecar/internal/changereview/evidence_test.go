package changereview

import (
	"encoding/json"
	"strings"
	"testing"
)

// Evidence: ids are "kind:ref", data is canonical JSON, the digest covers
// id, label and data so a changed number is detectable.

func TestNewEvidenceCanonicalAndDigest(t *testing.T) {
	a := mustEvidence(t, "queryid", "-42", "Q1 2000 calls",
		map[string]any{"calls": 2000, "mean_ms": 0.25, "text": "SELECT 1"})
	if a.ID != "queryid:-42" || a.Kind != "queryid" || a.Label != "Q1 2000 calls" {
		t.Fatalf("evidence = %+v", a)
	}
	if string(a.Data) != `{"calls":2000,"mean_ms":0.25,"text":"SELECT 1"}` {
		t.Fatalf("data %s is not canonical (sorted keys, compact)", a.Data)
	}
	if len(a.Digest) != 64 || strings.Trim(a.Digest, "0123456789abcdef") != "" {
		t.Fatalf("digest %q is not hex sha256", a.Digest)
	}
	b := mustEvidence(t, "queryid", "-42", "Q1 2000 calls",
		json.RawMessage(`{ "text":"SELECT 1", "mean_ms":0.25 ,"calls":2000}`))
	if b.Digest != a.Digest || string(b.Data) != string(a.Data) {
		t.Fatalf("the same data in another key order digests differently: %s vs %s",
			a.Digest, b.Digest)
	}
	if !a.Verify() {
		t.Fatal("fresh evidence does not verify")
	}
	tampered := a
	tampered.Data = json.RawMessage(`{"calls":20,"mean_ms":0.25,"text":"SELECT 1"}`)
	if tampered.Verify() {
		t.Fatal("evidence with a changed number still verifies")
	}
	relabeled := a
	relabeled.Label = "Q1 20 calls"
	if relabeled.Verify() {
		t.Fatal("evidence with a changed label still verifies")
	}
}

func TestNewEvidenceRejectsBadInput(t *testing.T) {
	cases := map[string][3]string{
		"empty kind":      {"", "1", "l"},
		"upper kind":      {"Stmt", "1", "l"},
		"empty ref":       {"stmt", "", "l"},
		"space in ref":    {"stmt", "a b", "l"},
		"newline in ref":  {"stmt", "a\nb", "l"},
		"long ref":        {"stmt", strings.Repeat("x", 300), "l"},
		"colon kind only": {"stmt:", "1", "l"},
	}
	for name, c := range cases {
		if _, err := NewEvidence(c[0], c[1], c[2], map[string]any{}); err == nil {
			t.Errorf("%s: NewEvidence(%q, %q) accepted", name, c[0], c[1])
		}
	}
	if _, err := NewEvidence("stmt", "1", "l", func() {}); err == nil {
		t.Error("unmarshalable data accepted")
	}
	if _, err := NewEvidence("stmt", "1", "l", json.RawMessage(`{bad`)); err == nil {
		t.Error("invalid raw JSON accepted")
	}
}

func TestNewEvidenceNilData(t *testing.T) {
	e := mustEvidence(t, "stmt", "1", "no data", nil)
	if string(e.Data) != "null" || !e.Verify() {
		t.Fatalf("nil data = %s (verify %v), want null and verifying", e.Data, e.Verify())
	}
}

func TestCanonicalJSONKeepsNumbers(t *testing.T) {
	got, err := CanonicalJSON(json.RawMessage(`{"b":12345678901234567890,"a":[1.50,2]}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"a":[1.50,2],"b":12345678901234567890}` {
		t.Fatalf("canonical = %s, want numbers kept as written", got)
	}
}

func TestEvidenceIDValidation(t *testing.T) {
	for id, ok := range map[string]bool{
		"stmt:1": true, "queryid:-42": true, "table:rv.rv_orders": true,
		"diff:app/models/order.rb:10-20": true, "": false, "stmt": false, ":1": false,
		"Stmt:1": false, "stmt:a b": false, "stmt:" + strings.Repeat("x", 200): false,
	} {
		if got := ValidEvidenceID(id); got != ok {
			t.Errorf("ValidEvidenceID(%q) = %v, want %v", id, got, ok)
		}
	}
}
