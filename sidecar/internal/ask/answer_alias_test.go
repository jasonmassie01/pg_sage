package ask

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/agentloop"
)

// mcp-05: a citation is the read a claim cited (by alias), not whatever
// read of the same durable id came last. Unit tests: no database, no
// model. No concurrency tests: compose is a pure function of the result.

func reReadResult(claims ...agentloop.Claim) agentloop.Result {
	return agentloop.Result{Final: json.RawMessage(`{"claims":[]}`), Claims: claims,
		Evidence: map[string]agentloop.Evidence{
			"E1": ev("queries:calls", "top 10 by calls: 4200"),
			"E2": ev("queries:calls", "top 50 by calls: 4200, 9100"),
		},
		Transcript: agentloop.Transcript{Stop: agentloop.StopFinal}}
}

func TestCompose_ReReadCitesTheEarlierReadsDigest(t *testing.T) {
	// Boundary: the same list read with limit 10, then 50; the claim
	// cites the first read.
	a := compose("testdb", reReadResult(agentloop.Claim{Text: "The top query ran 4200 calls.",
		EvidenceIDs: []string{"queries:calls"}, Aliases: []string{"E1"}}), nil)
	if len(a.Citations) != 1 {
		t.Fatalf("citations = %+v", a.Citations)
	}
	c := a.Citations[0]
	if c.Digest != digestOf("top 10 by calls: 4200") || c.Alias != "E1" ||
		c.ID != "queries:calls" {
		t.Fatalf("citation = %+v, want the limit-10 read (alias E1)", c)
	}
	st := a.Statements[0]
	if strings.Join(st.Aliases, ",") != "E1" || strings.Join(st.Citations, ",") !=
		"queries:calls" {
		t.Fatalf("statement = %+v", st)
	}
	if !strings.Contains(a.Text, "The top query ran 4200 calls. [1]") {
		t.Fatalf("text = %q", a.Text)
	}
}

func TestCompose_TwoReadsOfOneIDAreTwoCitations(t *testing.T) {
	a := compose("testdb", reReadResult(
		agentloop.Claim{Text: "A ran 4200 calls.", EvidenceIDs: []string{"queries:calls"},
			Aliases: []string{"E1", "E2"}},
		agentloop.Claim{Text: "B ran 9100 calls.", EvidenceIDs: []string{"queries:calls"},
			Aliases: []string{"E2"}}), nil)
	if len(a.Citations) != 2 || a.Citations[0].Alias != "E1" || a.Citations[1].Alias != "E2" {
		t.Fatalf("citations = %+v, want E1 then E2", a.Citations)
	}
	if a.Citations[0].Digest == a.Citations[1].Digest ||
		a.Citations[1].Digest != digestOf("top 50 by calls: 4200, 9100") {
		t.Fatalf("digests = %s / %s, want each read's own", a.Citations[0].Digest,
			a.Citations[1].Digest)
	}
	if !strings.Contains(a.Text, "A ran 4200 calls. [1] [2]") ||
		!strings.Contains(a.Text, "B ran 9100 calls. [2]") {
		t.Fatalf("text = %q, want citations numbered by alias", a.Text)
	}
}

func TestCompose_ClaimWithAnAliasTheRunDidNotIssueIsDropped(t *testing.T) {
	// Invalid input: a claim whose aliases the result does not hold (a
	// bug or a tampered result), or that carries no aliases at all, never
	// becomes a citation, even when its durable id was read.
	a := compose("testdb", reReadResult(
		agentloop.Claim{Text: "x", EvidenceIDs: []string{"queries:calls"},
			Aliases: []string{"E1", "E7"}},
		agentloop.Claim{Text: "y", EvidenceIDs: []string{"queries:calls"}}), nil)
	if len(a.Statements) != 0 || len(a.Citations) != 0 || len(a.Dropped) != 2 ||
		a.Dropped[0].Reason != agentloop.DropUnknownEvidence ||
		a.Dropped[1].Reason != agentloop.DropUnknownEvidence {
		t.Fatalf("answer = %+v", a)
	}
}

func TestCompose_NilEvidenceKeepsNoClaims(t *testing.T) {
	res := reReadResult(agentloop.Claim{Text: "x", EvidenceIDs: []string{"queries:calls"},
		Aliases: []string{"E1"}})
	res.Evidence = nil
	if a := compose("testdb", res, nil); len(a.Statements) != 0 ||
		a.Status != StatusNotObserved {
		t.Fatalf("answer = %+v", a)
	}
}

func TestAnswerJSON_AliasFieldsAreAdditive(t *testing.T) {
	a := compose("testdb", reReadResult(agentloop.Claim{Text: "x 4200.",
		EvidenceIDs: []string{"queries:calls"}, Aliases: []string{"E1"}}), nil)
	raw, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for _, want := range []string{`"alias":"E1"`, `"aliases":["E1"]`,
		`"citations":["queries:calls"]`, `"id":"queries:calls"`} {
		if !strings.Contains(s, want) {
			t.Errorf("answer JSON lacks %s: %s", want, s)
		}
	}
	// A stored answer from before the change (no alias fields) decodes.
	var old Answer
	if err := json.Unmarshal([]byte(`{"statements":[{"text":"t","citations":["finding:1"]}],`+
		`"citations":[{"id":"finding:1","kind":"finding","ref":"1"}]}`), &old); err != nil ||
		old.Citations[0].Alias != "" || len(old.Statements[0].Aliases) != 0 {
		t.Fatalf("old answer = %+v (%v)", old, err)
	}
}
