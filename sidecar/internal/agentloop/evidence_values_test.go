package agentloop

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sync"
	"testing"
)

// env-race-b: Evidence.Values is a slice, so a copy of an Evidence
// shares its backing array. A tool that rewrites a value it resolved, or
// the evidence it returned, must not change what the run grounds claims
// against, and a goroutine it leaves behind must not race the run.

var numberToken = regexp.MustCompile(`\d+`)

// valuesGround accepts a claim only when every number in it is one of the
// cited evidence's typed values.
func valuesGround(text string, cited []Evidence) error {
	for _, n := range numberToken.FindAllString(text, -1) {
		found := false
		for _, ev := range cited {
			found = found || slices.Contains(ev.Values, n)
		}
		if !found {
			return fmt.Errorf("number %s is not in the cited evidence", n)
		}
	}
	return nil
}

func valuesConfig(tools ...Tool) Config {
	cfg := baseConfig(tools...)
	cfg.Seed = []Evidence{{ID: "seed-1", Digest: "s1", Label: "lock_graph ok",
		Text: "pid 4242 blocks sessions for 90 s", Values: []string{"90"}}}
	cfg.Ground = valuesGround
	return cfg
}

func keptTexts(claims []Claim) []string {
	var out []string
	for _, c := range claims {
		out = append(out, c.Text)
	}
	return out
}

// Invalid input from a tool: rewriting a resolved alias's values.
func TestResolveEvidence_ToolCannotRewriteTheRunsValues(t *testing.T) {
	forge := Tool{Name: "forge", Description: "rewrites what it resolved",
		Parameters: json.RawMessage(`{"type":"object"}`),
		Run: func(ctx context.Context, _ json.RawMessage) (Output, error) {
			ev, ok := ResolveEvidence(ctx, "E1")
			if !ok || len(ev.Values) != 1 {
				t.Errorf("forge resolved %+v, %v; want E1 with one value", ev, ok)
				return Output{Status: "ok", Text: "nothing"}, nil
			}
			ev.Values[0] = "999"
			return Output{Status: "ok", Text: "forged"}, nil
		}}
	cfg := valuesConfig(forge)
	m := newScript(callsTools("forge", `{}`), callsTools("submit_conclusion",
		finalArgs("agree",
			Claim{Text: "sessions wait 999 s.", EvidenceIDs: []string{"E1"}},
			Claim{Text: "sessions wait 90 s.", EvidenceIDs: []string{"E1"}})))
	res, err := Run(context.Background(), m, cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := keptTexts(res.Claims); len(got) != 1 || got[0] != "sessions wait 90 s." {
		t.Fatalf("kept claims = %q; want only the 90 s claim (the forged 999 must not "+
			"ground)", got)
	}
	if v := res.Evidence["E1"].Values; len(v) != 1 || v[0] != "90" {
		t.Fatalf("run evidence E1 values = %q; want [90]", v)
	}
	if v := cfg.Seed[0].Values; v[0] != "90" {
		t.Fatalf("the caller's seed values = %q; want [90]", v)
	}
}

// State transition: evidence a tool returned is frozen once it is cited,
// even when the tool keeps the pointer and rewrites it on a later call.
func TestRun_ToolCannotRewriteCitedValuesAfterReturning(t *testing.T) {
	var kept *Evidence
	probe := Tool{Name: "probe", Description: "keeps its evidence pointer",
		Parameters: json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"}}}`),
		Run: func(_ context.Context, args json.RawMessage) (Output, error) {
			if kept != nil {
				kept.Values[0] = "999"
				return Output{Status: "ok", Text: "rewrote"}, nil
			}
			kept = &Evidence{ID: "probe-ev-1", Digest: "p1", Label: "probe ok",
				Text: "7 waiters", Values: []string{"7"}}
			return Output{Status: "ok", Text: "7 waiters", Evidence: kept}, nil
		}}
	cfg := valuesConfig(probe)
	m := newScript(callsTools("probe", `{"n":1}`), callsTools("probe", `{"n":2}`),
		callsTools("submit_conclusion", finalArgs("agree",
			Claim{Text: "999 waiters.", EvidenceIDs: []string{"E2"}},
			Claim{Text: "7 waiters.", EvidenceIDs: []string{"E2"}})))
	res, err := Run(context.Background(), m, cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := keptTexts(res.Claims); len(got) != 1 || got[0] != "7 waiters." {
		t.Fatalf("kept claims = %q; want only the 7 waiters claim", got)
	}
	if v := res.Evidence["E2"].Values; len(v) != 1 || v[0] != "7" {
		t.Fatalf("run evidence E2 values = %q; want [7]", v)
	}
}

// Two resolutions of one alias are independent copies: a tool's helper
// goroutines cannot change each other's view.
func TestResolveEvidence_EachResolutionIsItsOwnCopy(t *testing.T) {
	ctx := withAliases(context.Background(), map[string]Evidence{
		"E1": {ID: "seed-1", Values: []string{"90", "2"}}})
	first, _ := ResolveEvidence(ctx, "E1")
	first.Values[0] = "999"
	second, ok := ResolveEvidence(ctx, "E1")
	if !ok || !slices.Equal(second.Values, []string{"90", "2"}) {
		t.Fatalf("second resolution = %q, %v; want [90 2]", second.Values, ok)
	}
}

// Nil/empty: evidence without values stays without values (nil, not an
// empty slice), so the grounding fallback to Text is unchanged.
func TestResolveEvidence_NoValuesStaysNil(t *testing.T) {
	ctx := withAliases(context.Background(), map[string]Evidence{"E1": {ID: "seed-1"}})
	ev, ok := ResolveEvidence(ctx, "E1")
	if !ok || ev.Values != nil {
		t.Fatalf("resolved %+v, %v; want E1 with nil Values", ev, ok)
	}
}

// Concurrency: goroutines a tool leaves behind keep rewriting the values
// they resolved while the run issues aliases and grounds the answer. Run
// it with -race on a cgo host (ci-19); without the detector the result
// still has to be the run's own values.
func TestRun_LeftBehindGoroutinesRewritingValuesDoNotRaceTheRun(t *testing.T) {
	stop := make(chan struct{})
	var wg sync.WaitGroup
	spray := Tool{Name: "spray", Description: "leaves writers behind",
		Parameters: json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"}}}`),
		Run: func(ctx context.Context, _ json.RawMessage) (Output, error) {
			for range 4 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for i := 0; ; i++ {
						select {
						case <-stop:
							return
						default:
						}
						if ev, ok := ResolveEvidence(ctx, "E1"); ok {
							ev.Values[0] = fmt.Sprint(i)
						}
					}
				}()
			}
			return Output{Status: "ok", Text: "sprayed"}, nil
		}}
	lookup := &countingTool{}
	cfg := valuesConfig(spray, lookup.tool("lookup", 1, true, "lookup ok"))
	m := newScript(callsTools("spray", `{"n":1}`), callsTools("lookup", `{"n":1}`),
		callsTools("submit_conclusion", finalArgs("agree",
			Claim{Text: "sessions wait 90 s.", EvidenceIDs: []string{"E1"}})))
	res, err := Run(context.Background(), m, cfg)
	close(stop)
	wg.Wait()
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Claims) != 1 || !slices.Equal(res.Evidence["E1"].Values, []string{"90"}) {
		t.Fatalf("claims = %+v, E1 values = %q; want the 90 s claim on [90]",
			res.Claims, res.Evidence["E1"].Values)
	}
}

// State transition: the run copies its seeds when it starts, so a caller
// that still holds the seed slice and rewrites it mid-run cannot change
// what the run grounds claims against.
func TestRun_CallerCannotRewriteSeedValuesMidRun(t *testing.T) {
	var cfg Config
	rewrite := Tool{Name: "rewrite", Description: "the caller rewrites its seed",
		Parameters: json.RawMessage(`{"type":"object"}`),
		Run: func(context.Context, json.RawMessage) (Output, error) {
			cfg.Seed[0].Values[0] = "999"
			return Output{Status: "ok", Text: "rewrote the seed"}, nil
		}}
	cfg = valuesConfig(rewrite)
	m := newScript(callsTools("rewrite", `{}`), callsTools("submit_conclusion",
		finalArgs("agree",
			Claim{Text: "sessions wait 999 s.", EvidenceIDs: []string{"E1"}},
			Claim{Text: "sessions wait 90 s.", EvidenceIDs: []string{"E1"}})))
	res, err := Run(context.Background(), m, cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := keptTexts(res.Claims); len(got) != 1 || got[0] != "sessions wait 90 s." {
		t.Fatalf("kept claims = %q; want only the 90 s claim", got)
	}
	if v := res.Evidence["E1"].Values; len(v) != 1 || v[0] != "90" {
		t.Fatalf("run evidence E1 values = %q; want [90]", v)
	}
}
