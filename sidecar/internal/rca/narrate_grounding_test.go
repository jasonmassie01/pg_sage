package rca

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/sre/causal"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// sre-03: narration shares the investigator's grounding rule, so a probe
// result's head line, row ordinals and timestamp parts ground nothing.
// No concurrency test: parseNarration is pure over its evidence set.

func probeOnlyEvidence(t *testing.T) evidenceSet {
	t.Helper()
	since := time.Date(2026, 11, 5, 3, 12, 45, 0, time.UTC)
	res := probes.Result{ProbeID: probes.LongTransactions, Version: "v1",
		Status: probes.StatusOK, Columns: []string{"pid", "xact_start", "xact_age_s"},
		Rows: []probes.Row{{"pid": int64(4242), "xact_start": since, "xact_age_s": 75.31},
			{"pid": int64(5001), "xact_start": since, "xact_age_s": 60.0}}}
	return newEvidenceSet(Incident{}, incidentEvidence{
		observations: []causal.Observation{{EvidenceID: "P1", Result: res}}})
}

func TestParseNarration_ProbeFormattingGroundsNothing(t *testing.T) {
	ev := probeOnlyEvidence(t)
	for summary, num := range map[string]string{
		"pid 4242 blocks 2 sessions.":      "2",
		"pid 4242 has waited 12 minutes.":  "12",
		"pid 4242 has idled 45 seconds.":   "45",
		"1 transaction is open since 3am.": "1",
	} {
		final := `{"summary": "` + summary + `", "evidence_ids": ["P1"]}`
		_, err := parseNarration(final, ev)
		if !errors.Is(err, sre.ErrUngroundedNumber) || !strings.Contains(err.Error(), num) {
			t.Errorf("%q: err = %v, want ungrounded number %s", summary, err, num)
		}
	}
}

func TestParseNarration_ProbeValuesStillGround(t *testing.T) {
	final := `{"claims": [{"text": "pid 4242 opened its transaction at ` +
		`2026-11-05T03:12:45Z and has been open 75.31 s.", "evidence_ids": ["P1"]}, ` +
		`{"text": "pid 5001 has been open 60 s.", "evidence_ids": ["P1"]}]}`
	n, err := parseNarration(final, probeOnlyEvidence(t))
	if err != nil || n.Source != NarrationLLM || len(n.Citations) != 1 ||
		n.Citations[0] != "P1" || !strings.Contains(n.Text, "75.31") {
		t.Fatalf("narration = %+v, err %v", n, err)
	}
}
