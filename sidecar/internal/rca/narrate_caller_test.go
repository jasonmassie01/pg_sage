package rca

import (
	"context"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// sre-06: narration probes are tagged with their caller class so the
// probe queue metrics tell narration and log RCA from investigations.

type callerProbes struct {
	mu      sync.Mutex
	callers []probes.Caller
}

func (c *callerProbes) Run(ctx context.Context, id probes.ID, _ probes.Args) probes.Result {
	c.mu.Lock()
	c.callers = append(c.callers, probes.CallerOf(ctx))
	c.mu.Unlock()
	return probes.Result{ProbeID: id, Version: "v1", Status: probes.StatusEmpty,
		Reason: "no_rows"}
}

func TestGatherEvidence_TagsCallerBySource(t *testing.T) {
	for source, want := range map[string]probes.Caller{
		"deterministic":     probes.CallerNarration,
		"log_deterministic": probes.CallerLogRCA,
		"":                  probes.CallerNarration,
	} {
		fp := &callerProbes{}
		eng := testEngine()
		eng.WithProbes(fp)
		inc := lockIncident(t)
		inc.Source = source
		eng.gatherEvidence(context.Background(), inc)
		if len(fp.callers) == 0 {
			t.Fatalf("source %q: no probes ran", source)
		}
		for _, got := range fp.callers {
			if got != want {
				t.Fatalf("source %q: caller = %q, want %q", source, got, want)
			}
		}
	}
}
