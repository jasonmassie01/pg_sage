package agentloop

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// The citation filter: a claim survives only when every evidence id it
// cites is evidence the run holds (seeded or produced by a tool call)
// and the caller's grounding check accepts it. Everything else is
// dropped and counted, never repaired.

func catalog() map[string]Evidence {
	return map[string]Evidence{
		"E1": {ID: "ev-1", Text: "lock_graph ok: pid 4242 blocks 2 sessions for 90 s"},
		"E2": {ID: "ev-2", Text: "long_transactions ok: xact age 90 s"},
	}
}

func numbersIn(text string, cited []Evidence) error {
	for _, w := range strings.Fields(text) {
		w = strings.Trim(w, ".,;")
		if w == "" || w[0] < '0' || w[0] > '9' {
			continue
		}
		found := false
		for _, e := range cited {
			found = found || strings.Contains(e.Text, w)
		}
		if !found {
			return errors.New("ungrounded number " + w)
		}
	}
	return nil
}

func TestFilterClaims_KeepsCitedGroundedClaimsWithDurableIDs(t *testing.T) {
	kept, dropped := FilterClaims([]Claim{
		{Text: "pid 4242 blocks 2 sessions.", EvidenceIDs: []string{"E1"}},
		{Text: "The transaction is 90 s old.", EvidenceIDs: []string{"E2", "E1", "E2"}},
	}, catalog(), numbersIn, 5)
	if len(dropped) != 0 || len(kept) != 2 {
		t.Fatalf("kept %+v dropped %+v", kept, dropped)
	}
	if strings.Join(kept[0].EvidenceIDs, ",") != "ev-1" ||
		strings.Join(kept[1].EvidenceIDs, ",") != "ev-2,ev-1" {
		t.Fatalf("evidence ids = %v / %v, want durable ids, each once", kept[0].EvidenceIDs,
			kept[1].EvidenceIDs)
	}
}

func TestFilterClaims_DropsEachKindOfBadClaimWithItsReason(t *testing.T) {
	cases := []struct {
		claim  Claim
		reason string
	}{
		{Claim{Text: "Locks cause it.", EvidenceIDs: nil}, DropUncited},
		{Claim{Text: "Locks cause it.", EvidenceIDs: []string{}}, DropUncited},
		{Claim{Text: "pid 4242 blocks.", EvidenceIDs: []string{"E9"}}, DropUnknownEvidence},
		{Claim{Text: "pid 4242 blocks.", EvidenceIDs: []string{"E1", "E99"}},
			DropUnknownEvidence},
		{Claim{Text: "pid 4242 blocks.", EvidenceIDs: []string{"ev-1"}}, DropUnknownEvidence},
		{Claim{Text: "It blocks 987654 sessions.", EvidenceIDs: []string{"E1"}}, DropUngrounded},
		{Claim{Text: "   ", EvidenceIDs: []string{"E1"}}, DropEmpty},
		{Claim{Text: strings.Repeat("x", MaxClaimRunes+1), EvidenceIDs: []string{"E1"}},
			DropTooLong},
	}
	for _, tc := range cases {
		kept, dropped := FilterClaims([]Claim{tc.claim}, catalog(), numbersIn, 5)
		if len(kept) != 0 || len(dropped) != 1 || dropped[0].Reason != tc.reason {
			t.Errorf("claim %+v: kept %+v dropped %+v, want dropped %s", tc.claim, kept,
				dropped, tc.reason)
		}
	}
}

func TestFilterClaims_DuplicatesAndOverflowAreDropped(t *testing.T) {
	c := Claim{Text: "pid 4242 blocks 2 sessions.", EvidenceIDs: []string{"E1"}}
	dup := Claim{Text: "  PID 4242 blocks   2 sessions. ", EvidenceIDs: []string{"E1"}}
	kept, dropped := FilterClaims([]Claim{c, dup}, catalog(), nil, 5)
	if len(kept) != 1 || len(dropped) != 1 || dropped[0].Reason != DropDuplicate {
		t.Fatalf("kept %+v dropped %+v", kept, dropped)
	}
	many := []Claim{}
	for _, text := range []string{"a 4242.", "b 4242.", "c 4242."} {
		many = append(many, Claim{Text: text, EvidenceIDs: []string{"E1"}})
	}
	kept, dropped = FilterClaims(many, catalog(), nil, 2)
	if len(kept) != 2 || len(dropped) != 1 || dropped[0].Reason != DropOverLimit {
		t.Fatalf("kept %+v dropped %+v, want 2 kept and 1 over_limit", kept, dropped)
	}
}

func TestFilterClaims_NilAndEmptyInputs(t *testing.T) {
	if kept, dropped := FilterClaims(nil, catalog(), nil, 5); kept != nil || dropped != nil {
		t.Fatalf("nil claims: kept %+v dropped %+v", kept, dropped)
	}
	kept, dropped := FilterClaims([]Claim{{Text: "x 1.", EvidenceIDs: []string{"E1"}}},
		nil, nil, 5)
	if len(kept) != 0 || len(dropped) != 1 || dropped[0].Reason != DropUnknownEvidence {
		t.Fatalf("no evidence: kept %+v dropped %+v", kept, dropped)
	}
}

func TestRun_HallucinatedEvidenceIsDroppedAndCounted(t *testing.T) {
	m := newScript(callsTools("submit_conclusion", finalArgs("contest",
		Claim{Text: "pid 4242 blocks 2 sessions.", EvidenceIDs: []string{"E1"}},
		Claim{Text: "The archiver failed 12 times.", EvidenceIDs: []string{"E7"}},
		Claim{Text: "Memory pressure.", EvidenceIDs: nil})))
	cfg := baseConfig()
	cfg.Ground = numbersIn
	res, err := Run(context.Background(), m, cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Claims) != 1 || len(res.Dropped) != 2 {
		t.Fatalf("claims %+v dropped %+v", res.Claims, res.Dropped)
	}
	reasons := res.Dropped[0].Reason + "," + res.Dropped[1].Reason
	if reasons != DropUnknownEvidence+","+DropUncited {
		t.Fatalf("drop reasons = %s", reasons)
	}
}

func TestRun_ClaimsMayCiteEvidenceFromToolCalls(t *testing.T) {
	tool := &countingTool{}
	m := newScript(callsTools("lookup", `{}`), callsTools("submit_conclusion",
		finalArgs("agree", Claim{Text: "There are 7 waiters.", EvidenceIDs: []string{"E2"}},
			Claim{Text: "There are 8 waiters.", EvidenceIDs: []string{"E2"}})))
	cfg := baseConfig(tool.tool("lookup", 1, true, "lookup ok: 7 waiters"))
	cfg.Ground = numbersIn
	res, err := Run(context.Background(), m, cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Claims) != 1 || res.Claims[0].EvidenceIDs[0] != "lookup-ev-1" ||
		len(res.Dropped) != 1 || res.Dropped[0].Reason != DropUngrounded {
		t.Fatalf("claims %+v dropped %+v", res.Claims, res.Dropped)
	}
}

func TestRun_NonCitableToolResultsCannotBeCited(t *testing.T) {
	free := &countingTool{}
	m := newScript(callsTools("graph", `{}`), callsTools("submit_conclusion",
		finalArgs("agree", Claim{Text: "The graph says so.", EvidenceIDs: []string{"E2"}})))
	res, err := Run(context.Background(), m, baseConfig(free.tool("graph", 0, false, "y")))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Claims) != 0 || len(res.Dropped) != 1 ||
		res.Dropped[0].Reason != DropUnknownEvidence {
		t.Fatalf("claims %+v dropped %+v", res.Claims, res.Dropped)
	}
}
