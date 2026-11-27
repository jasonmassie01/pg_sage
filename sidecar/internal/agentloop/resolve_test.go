package agentloop

import (
	"context"
	"encoding/json"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ResolveEvidence (mcp-02): a tool running inside a run can turn an
// alias the run issued (E1, E2, ...) into its evidence, so a write tool
// can take the aliases the protocol tells the model to cite. Outside a
// run nothing resolves, and a tool sees a copy taken when it was called.

func TestResolveEvidence_NoRunResolvesNothing(t *testing.T) {
	for name, ctx := range map[string]context.Context{"background": context.Background(),
		"nil": nil, "canceled": canceledCtx()} {
		for _, ref := range []string{"E1", "", "seed-1"} {
			if ev, ok := ResolveEvidence(ctx, ref); ok || !reflect.DeepEqual(ev, Evidence{}) {
				t.Errorf("%s ctx: ResolveEvidence(%q) = %+v, %v; want zero, false",
					name, ref, ev, ok)
			}
		}
	}
}

func canceledCtx() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// resolveProbe is a tool that resolves refs inside the run and records
// what it saw. It is citable so that it takes an alias itself.
type resolveProbe struct {
	mu   sync.Mutex
	refs []string
	seen []map[string]Evidence
}

func (p *resolveProbe) tool() Tool {
	return Tool{Name: "probe", Description: "resolves aliases",
		Parameters: json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"}}}`),
		Run: func(ctx context.Context, _ json.RawMessage) (Output, error) {
			got := map[string]Evidence{}
			for _, ref := range p.refs {
				if ev, ok := ResolveEvidence(ctx, ref); ok {
					got[ref] = ev
				}
			}
			p.mu.Lock()
			p.seen = append(p.seen, got)
			p.mu.Unlock()
			return Output{Status: "ok", Text: "probe ok", Evidence: &Evidence{
				ID: "probe-ev", Digest: "pd", Label: "probe ok", Text: "probe ok"}}, nil
		}}
}

func TestResolveEvidence_ToolSeesAliasesIssuedBeforeItsCall(t *testing.T) {
	lookup := &countingTool{}
	probe := &resolveProbe{refs: []string{"E1", "E2", "E3", "E4", "e1", " E1", "seed-1"}}
	m := newScript(callsTools("lookup", `{"n":1}`), callsTools("probe", `{"n":1}`),
		callsTools("submit_conclusion", finalArgs("agree")))
	cfg := baseConfig(lookup.tool("lookup", 1, true, "lookup ok: 7 waiters"), probe.tool())
	if _, err := Run(context.Background(), m, cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(probe.seen) != 1 {
		t.Fatalf("probe ran %d times, want 1", len(probe.seen))
	}
	got := probe.seen[0]
	if got["E1"].ID != "seed-1" || got["E1"].Digest != "s1" ||
		got["E2"].ID != "lookup-ev-1" || got["E2"].Digest != "d1" ||
		got["E2"].Label != "lookup ok" || got["E2"].Text != "lookup ok: 7 waiters" {
		t.Fatalf("resolved = %+v, want E1 = seed-1 and E2 = lookup-ev-1", got)
	}
	// E3 is the probe's own alias: issued only after it returns. Aliases
	// are exact; durable ids are not aliases.
	if len(got) != 2 {
		t.Fatalf("resolved %d refs (%+v), want only E1 and E2", len(got), got)
	}
}

// A tool cannot change the run's aliases through what it resolved, and a
// context it keeps past its call neither sees later aliases nor races the
// run's map while the run issues them.
func TestResolveEvidence_ToolCopyCannotChangeTheRun(t *testing.T) {
	var kept atomic.Value
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var leakedLater atomic.Bool
	grab := Tool{Name: "grab", Description: "keeps its context",
		Parameters: json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"}}}`),
		Run: func(ctx context.Context, _ json.RawMessage) (Output, error) {
			ev, ok := ResolveEvidence(ctx, "E1")
			if !ok {
				t.Error("grab could not resolve E1")
			}
			ev.ID, ev.Digest = "forged", "forged"
			kept.Store(ctx)
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-stop:
						return
					default:
					}
					if _, ok := ResolveEvidence(ctx, "E3"); ok {
						leakedLater.Store(true)
					}
					time.Sleep(time.Millisecond)
				}
			}()
			return Output{Status: "ok", Text: "grab ok"}, nil
		}}
	lookup := &countingTool{}
	m := newScript(callsTools("grab", `{"n":1}`), callsTools("lookup", `{"n":1}`),
		callsTools("lookup", `{"n":2}`),
		callsTools("submit_conclusion", finalArgs("agree",
			Claim{Text: "pid 4242 blocks 2 sessions.", EvidenceIDs: []string{"E1"}},
			Claim{Text: "the lookup confirms 7 waiters.", EvidenceIDs: []string{"E3"}})))
	cfg := baseConfig(grab, lookup.tool("lookup", 1, true, "lookup ok: 7 waiters"))
	res, err := Run(context.Background(), m, cfg)
	close(stop)
	wg.Wait()
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Claims) != 2 || res.Claims[0].EvidenceIDs[0] != "seed-1" ||
		res.Claims[1].EvidenceIDs[0] != "lookup-ev-2" {
		t.Fatalf("claims = %+v, want E1 = seed-1 and E3 = lookup-ev-2", res.Claims)
	}
	if leakedLater.Load() {
		t.Fatal("a context kept past its call resolved an alias issued later")
	}
	ctx, _ := kept.Load().(context.Context)
	if ev, ok := ResolveEvidence(ctx, "E1"); !ok || ev.ID != "seed-1" {
		t.Fatalf("kept context resolves E1 to %+v, %v; want seed-1", ev, ok)
	}
}
