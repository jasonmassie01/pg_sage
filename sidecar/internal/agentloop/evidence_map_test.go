package agentloop

import (
	"context"
	"errors"
	"testing"
)

// Result.Evidence (Change Review reasoner): every alias the run held, so a
// caller can resolve aliases cited in final fields other than claims
// (statements, findings). It holds the seed, every citable tool result
// and nothing that was refused, failed or not citable.

func TestRun_ResultEvidenceMapsEveryAliasTheRunHeld(t *testing.T) {
	cited, plain := &countingTool{}, &countingTool{}
	m := newScript(
		callsTools("lookup", `{"n":1}`, "note", `{"n":2}`, "lookup", `{"n":3}`),
		callsTools("submit_conclusion", finalArgs("agree")))
	cfg := baseConfig(cited.tool("lookup", 1, true, "lookup ok: 7 waiters"),
		plain.tool("note", 0, false, "not citable"))
	res, err := Run(context.Background(), m, cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := map[string]string{"E1": "seed-1", "E2": "lookup-ev-1", "E3": "lookup-ev-2"}
	if len(res.Evidence) != len(want) {
		t.Fatalf("evidence = %+v, want exactly %v", res.Evidence, want)
	}
	for alias, id := range want {
		if res.Evidence[alias].ID != id {
			t.Errorf("alias %s = %+v, want id %s", alias, res.Evidence[alias], id)
		}
	}
	if res.Evidence["E2"].Text != "lookup ok: 7 waiters" || res.Evidence["E2"].Digest != "d1" {
		t.Errorf("E2 lost its text or digest: %+v", res.Evidence["E2"])
	}
	for _, s := range res.Transcript.Steps {
		if s.Alias != "" && res.Evidence[s.Alias].ID != s.EvidenceID {
			t.Errorf("step %d alias %s resolves to %s, transcript says %s", s.Seq, s.Alias,
				res.Evidence[s.Alias].ID, s.EvidenceID)
		}
	}
}

func TestRun_ResultEvidenceIsTheSeedWhenNoToolRan(t *testing.T) {
	m := newScript(callsTools("submit_conclusion", finalArgs("done")))
	res, err := Run(context.Background(), m, baseConfig())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Evidence) != 1 || res.Evidence["E1"].ID != "seed-1" {
		t.Fatalf("evidence = %+v, want only the seed as E1", res.Evidence)
	}
}

func TestRun_ResultEvidenceIsEmptyNotNilWithoutSeedOrResults(t *testing.T) {
	cfg := baseConfig()
	cfg.Seed = nil
	res, err := Run(context.Background(), newScript(fails(errors.New("boom"))), cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Evidence == nil || len(res.Evidence) != 0 {
		t.Fatalf("evidence = %#v, want an empty map", res.Evidence)
	}
	if res.Transcript.Stop != StopProvider {
		t.Fatalf("stop = %q, want %q", res.Transcript.Stop, StopProvider)
	}
}

func TestRun_ResultEvidenceIsACopy(t *testing.T) {
	m := newScript(callsTools("submit_conclusion", finalArgs("done")))
	cfg := baseConfig()
	res, err := Run(context.Background(), m, cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	res.Evidence["E1"] = Evidence{ID: "changed"}
	res.Evidence["E9"] = Evidence{ID: "added"}
	again, err := Run(context.Background(), newScript(callsTools("submit_conclusion",
		finalArgs("done"))), cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if again.Evidence["E1"].ID != "seed-1" || len(again.Evidence) != 1 ||
		cfg.Seed[0].ID != "seed-1" {
		t.Fatalf("a caller's edit leaked: %+v, seed %+v", again.Evidence, cfg.Seed)
	}
}

// No concurrent access test: Result.Evidence is built by one run's own
// goroutine and handed to the caller once.
