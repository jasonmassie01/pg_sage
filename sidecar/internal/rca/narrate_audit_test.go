package rca

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/notify"
)

// Post-test audit additions for incident narration.

func TestDecorateEvents_EscalatedIsNarrated(t *testing.T) {
	s := newNarrServer(t, func(_ int, w http.ResponseWriter) {
		writeCompletion(w, groundedFinal, nil)
	})
	eng := narrEngine(s.srv.URL, true)
	inc := lockIncident(t)
	ev := notify.IncidentEscalatedEvent(incidentInfo(&inc))
	out := eng.decorateEvents(context.Background(),
		[]pendingEvent{{event: ev, incident: inc}})
	if out[0].Data["narrative_source"] != NarrationLLM ||
		!strings.Contains(out[0].Body, "Summary (LLM, cites E2)") {
		t.Fatalf("escalated event = %+v", out[0])
	}
}

// Without a dispatcher nothing would read a narrative, so persisting
// incidents must not spend LLM tokens.
func TestPersistIncidents_NoDispatcherNoNarration(t *testing.T) {
	pool, ctx := lifecycleDB(t)
	s := newNarrServer(t, func(_ int, w http.ResponseWriter) {
		writeCompletion(w, groundedFinal, nil)
	})
	eng, _ := lifecycleEngine(t, pool)
	withLLM := narrEngine(s.srv.URL, true)
	eng.cfg.NarrationEnabled = true
	eng.WithLLM(withLLM.llmClient)
	eng.ObserveLockChains(ctx,
		[]analyzer.Finding{chainFinding(4242, 3, 2, "idle in transaction")})
	if err := eng.PersistIncidents(ctx, pool); err != nil {
		t.Fatalf("persist: %v", err)
	}
	if n := s.calls.Load(); n != 0 {
		t.Fatalf("LLM called %d times with no dispatcher wired", n)
	}
}

// Narration runs after the state change is durable and a narration
// failure never blocks delivery: the event still goes out, labeled
// deterministic.
func TestPersistIncidents_NarrationFailureStillDelivers(t *testing.T) {
	pool, ctx := lifecycleDB(t)
	s := newNarrServer(t, func(_ int, w http.ResponseWriter) {
		w.WriteHeader(http.StatusTooManyRequests)
	})
	eng, db := lifecycleEngine(t, pool)
	eng.cfg.NarrationEnabled = true
	eng.WithLLM(narrEngine(s.srv.URL, true).llmClient)
	rec := &recordingDispatcher{}
	eng.WithDispatcher(rec)
	eng.ObserveLockChains(ctx,
		[]analyzer.Finding{chainFinding(4242, 3, 2, "idle in transaction")})
	if err := eng.PersistIncidents(ctx, pool); err != nil {
		t.Fatalf("persist: %v", err)
	}
	det := rec.byType("incident_detected")
	if len(det) != 1 || det[0].Data["database"] != db ||
		det[0].Data["narrative_source"] != NarrationDeterministic ||
		!strings.Contains(det[0].Data["narrative_fallback_reason"].(string),
			"rate limited") {
		t.Fatalf("detected events = %+v", det)
	}
}
