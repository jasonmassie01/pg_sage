package agentloop

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// mcp-05: two reads may share a durable id (the same list read with
// another limit). A claim keeps the aliases it cited, each read keeps
// its own evidence, and the run hands every alias's evidence back, so a
// citation shows what the claim was grounded on, not a later read.
// No concurrency tests: aliases are issued inside the run's one
// goroutine; resolve_test.go already covers the copy tools receive.

func sharedIDCatalog() map[string]Evidence {
	return map[string]Evidence{
		"E1": {ID: "queries:calls", Digest: "d-10", Text: "top 10: 4200 calls"},
		"E2": {ID: "ev-2", Digest: "d-2", Text: "long_transactions ok: xact age 90 s"},
		"E3": {ID: "queries:calls", Digest: "d-50", Text: "top 50: 9100 calls"},
	}
}

func TestFilterClaims_SameDurableIDKeepsEachCitedAlias(t *testing.T) {
	var grounded []Evidence
	ground := func(text string, cited []Evidence) error {
		grounded = append([]Evidence{}, cited...)
		return numbersIn(text, cited)
	}
	kept, dropped := FilterClaims([]Claim{{Text: "4200 then 9100 calls.",
		EvidenceIDs: []string{"E1", "E3"}}}, sharedIDCatalog(), ground, 5)
	if len(dropped) != 0 || len(kept) != 1 {
		t.Fatalf("kept %+v dropped %+v", kept, dropped)
	}
	if got := strings.Join(kept[0].Aliases, ","); got != "E1,E3" {
		t.Fatalf("aliases = %s, want E1,E3 (one per read)", got)
	}
	if got := strings.Join(kept[0].EvidenceIDs, ","); got != "queries:calls" {
		t.Fatalf("evidence ids = %s, want the durable id once", got)
	}
	if len(grounded) != 2 || grounded[0].Digest != "d-10" || grounded[1].Digest != "d-50" {
		t.Fatalf("grounded on %+v, want both reads in cited order", grounded)
	}
}

func TestFilterClaims_AliasesKeepCitedOrderEachOnce(t *testing.T) {
	kept, _ := FilterClaims([]Claim{{Text: "The transaction is 90 s old.",
		EvidenceIDs: []string{"E2", "E1", "E2"}}}, catalog(), numbersIn, 5)
	if len(kept) != 1 || strings.Join(kept[0].Aliases, ",") != "E2,E1" ||
		strings.Join(kept[0].EvidenceIDs, ",") != "ev-2,ev-1" {
		t.Fatalf("kept %+v, want aliases E2,E1 and ids ev-2,ev-1", kept)
	}
}

func TestFilterClaims_DroppedClaimCarriesNoAliases(t *testing.T) {
	// Boundary: one known and one unknown alias drops the whole claim.
	kept, dropped := FilterClaims([]Claim{{Text: "x.", EvidenceIDs: []string{"E1", "E9"}}},
		sharedIDCatalog(), nil, 5)
	if len(kept) != 0 || len(dropped) != 1 || dropped[0].Reason != DropUnknownEvidence {
		t.Fatalf("kept %+v dropped %+v", kept, dropped)
	}
}

// sameIDTool answers every call with the same durable id; the text (and
// so the digest) depends on the limit asked for.
func sameIDTool() Tool {
	return Tool{Name: "top_queries", Description: "top queries", Cost: 1,
		Parameters: json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"}}}`),
		Run: func(_ context.Context, args json.RawMessage) (Output, error) {
			var a struct{ N int }
			if err := json.Unmarshal(args, &a); err != nil {
				return Output{}, err
			}
			text := "top " + itoa(a.N) + ": 4200 calls"
			if a.N > 10 {
				text = "top " + itoa(a.N) + ": 9100 calls"
			}
			return Output{Status: "ok", Text: text, Evidence: &Evidence{ID: "queries:calls",
				Digest: "digest-" + itoa(a.N), Label: "queries:calls ok", Text: text}}, nil
		}}
}

func TestRun_ReReadKeepsTheEvidenceTheClaimCited(t *testing.T) {
	m := newScript(callsTools("top_queries", `{"n":10}`),
		callsTools("top_queries", `{"n":50}`),
		callsTools("submit_conclusion", finalArgs("agree",
			Claim{Text: "The top query ran 4200 calls.", EvidenceIDs: []string{"E2"}})))
	cfg := baseConfig(sameIDTool())
	cfg.Ground = numbersIn
	res, err := Run(context.Background(), m, cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Claims) != 1 || strings.Join(res.Claims[0].Aliases, ",") != "E2" ||
		res.Claims[0].EvidenceIDs[0] != "queries:calls" {
		t.Fatalf("claims %+v dropped %+v", res.Claims, res.Dropped)
	}
	e2, e3 := res.Evidence["E2"], res.Evidence["E3"]
	if e2.Digest != "digest-10" || e3.Digest != "digest-50" || e2.ID != e3.ID {
		t.Fatalf("evidence E2 %+v E3 %+v, want each read's own digest", e2, e3)
	}
	if res.Evidence["E1"].ID != "seed-1" || len(res.Evidence) != 3 {
		t.Fatalf("evidence = %+v, want the seed and both reads", res.Evidence)
	}
}

func TestRun_ModelSuppliedAliasesAreIgnored(t *testing.T) {
	// Invalid input: the model cannot choose the aliases a claim keeps;
	// they come from the evidence_ids the filter resolved.
	final := `{"outcome":"agree","claims":[{"text":"pid 4242 blocks 2 sessions.",` +
		`"evidence_ids":["E1"],"aliases":["E9","__proto__"]}]}`
	m := newScript(callsTools("submit_conclusion", final))
	res, err := Run(context.Background(), m, baseConfig())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Claims) != 1 || strings.Join(res.Claims[0].Aliases, ",") != "E1" {
		t.Fatalf("claims = %+v, want aliases [E1]", res.Claims)
	}
}

func TestRun_EvidenceIsACopyAndEmptyWithoutReads(t *testing.T) {
	cfg := baseConfig()
	cfg.Seed = nil
	m := newScript(callsTools("submit_conclusion", finalArgs("agree")))
	res, err := Run(context.Background(), m, cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Evidence == nil || len(res.Evidence) != 0 {
		t.Fatalf("evidence = %#v, want an empty map", res.Evidence)
	}
	cfg = baseConfig()
	res, err = Run(context.Background(), newScript(callsTools("submit_conclusion",
		finalArgs("agree"))), cfg)
	if err != nil || res.Evidence["E1"].ID != "seed-1" {
		t.Fatalf("seeded run evidence = %+v (%v)", res.Evidence, err)
	}
	res.Evidence["E1"] = Evidence{ID: "tampered"}
	if cfg.Seed[0].ID != "seed-1" {
		t.Fatalf("changing the result changed the config's seed: %+v", cfg.Seed)
	}
}
