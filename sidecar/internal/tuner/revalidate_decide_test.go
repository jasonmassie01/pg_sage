package tuner

import (
	"strings"
	"testing"
	"time"
)

// Pure revalidation decisions (tuning-01). decideFromObservation turns one
// hint row and one pg_stat_statements observation into the hint's next
// state. A working hint (a fast query) stays installed, a stats reset
// re-baselines instead of breaking, a quiet query is dormant, and age only
// marks a hint due for re-verification.

// No concurrent access tests here: decideFromObservation is a pure function
// of its arguments. Revalidate alongside Tune is covered on the database
// (TestRevalidate_ConcurrentWithTune).

var revalNow = time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

func ptrInt64(v int64) *int64 { return &v }

func ptrTime(v time.Time) *time.Time { return &v }

func freshHint(checkpoint *int64, resetAt *time.Time) hintRow {
	return hintRow{
		ID:               7,
		QueryID:          42,
		Status:           "active",
		CreatedAt:        revalNow.Add(-time.Hour),
		CallsAtLastCheck: checkpoint,
		PgssResetAt:      resetAt,
	}
}

func observed(calls int64, meanMs float64, resetAt time.Time) pgssObservation {
	return pgssObservation{Calls: calls, MeanMs: meanMs, Found: true,
		ResetAt: ptrTime(resetAt)}
}

// A hinted query at 5 ms is what a working hint achieves: it stays active
// (the old check 4 retired it as "observational success").
func TestDecideFromObservation_FastQueryStaysActive(t *testing.T) {
	reset := revalNow.Add(-48 * time.Hour)
	h := freshHint(ptrInt64(100), ptrTime(reset))
	d := decideFromObservation(h, observed(150, 5, reset), 14*24*time.Hour, revalNow)
	if d.action != hintActionKeep {
		t.Fatalf("action = %v, want hintActionKeep (a fast hinted query keeps its hint)",
			d.action)
	}
	if d.nextStatus(h) != "active" {
		t.Fatalf("next status = %q, want active", d.nextStatus(h))
	}
	if d.checkpoint == nil || *d.checkpoint != 150 {
		t.Fatalf("checkpoint = %v, want 150", d.checkpoint)
	}
	if d.reverifyDue {
		t.Fatal("a one-hour-old hint must not be due for re-verification")
	}
}

// Boundary on the calls checkpoint: below (with a newer stats_reset) is a
// reset, equal is dormant, above is active.
func TestDecideFromObservation_CallsCheckpointBoundary(t *testing.T) {
	stored := revalNow.Add(-72 * time.Hour)
	newer := revalNow.Add(-time.Hour)
	cases := []struct {
		name       string
		calls      int64
		resetAt    time.Time
		action     hintAction
		status     string
		checkpoint int64
		reason     string
	}{
		{"below with newer stats_reset", 3, newer, hintActionReset, "active", 3, "stats_reset"},
		{"below without a newer stats_reset", 3, stored, hintActionReset, "active", 3,
			"stats_reset"},
		{"equal", 100, stored, hintActionDormant, "dormant", 100, "no new executions"},
		{"one above", 101, stored, hintActionKeep, "active", 101, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := freshHint(ptrInt64(100), ptrTime(stored))
			d := decideFromObservation(h, observed(tc.calls, 300, tc.resetAt), 0, revalNow)
			if d.action != tc.action {
				t.Fatalf("action = %v, want %v", d.action, tc.action)
			}
			if got := d.nextStatus(h); got != tc.status {
				t.Fatalf("next status = %q, want %q", got, tc.status)
			}
			if d.checkpoint == nil || *d.checkpoint != tc.checkpoint {
				t.Fatalf("checkpoint = %v, want %d", d.checkpoint, tc.checkpoint)
			}
			if !strings.Contains(d.reason, tc.reason) {
				t.Fatalf("reason %q, want it to contain %q", d.reason, tc.reason)
			}
			if d.resetAt == nil || !d.resetAt.Equal(tc.resetAt) {
				t.Fatalf("recorded stats_reset = %v, want %v", d.resetAt, tc.resetAt)
			}
		})
	}
}

// A newer stats_reset re-baselines even when calls climbed past the old
// checkpoint again before the pass ran (the counter restarted from zero).
func TestDecideFromObservation_NewerResetWithHigherCalls(t *testing.T) {
	stored := revalNow.Add(-72 * time.Hour)
	h := freshHint(ptrInt64(10), ptrTime(stored))
	d := decideFromObservation(h, observed(500, 300, revalNow.Add(-time.Minute)), 0, revalNow)
	if d.action != hintActionReset || d.checkpoint == nil || *d.checkpoint != 500 {
		t.Fatalf("decision = %+v, want a reset re-baselined to 500", d)
	}
}

// A reset while the hint is dormant keeps it dormant: the reset says
// nothing about whether the query runs.
func TestDecideFromObservation_ResetKeepsDormantStatus(t *testing.T) {
	stored := revalNow.Add(-72 * time.Hour)
	h := freshHint(ptrInt64(40), ptrTime(stored))
	h.Status = "dormant"
	d := decideFromObservation(h, observed(2, 300, revalNow.Add(-time.Hour)), 0, revalNow)
	if d.action != hintActionReset || d.nextStatus(h) != "dormant" {
		t.Fatalf("decision = %+v status %q, want a reset that keeps dormant",
			d, d.nextStatus(h))
	}
}

// A dormant hint whose query runs again returns to active.
func TestDecideFromObservation_DormantResumes(t *testing.T) {
	reset := revalNow.Add(-72 * time.Hour)
	h := freshHint(ptrInt64(40), ptrTime(reset))
	h.Status = "dormant"
	d := decideFromObservation(h, observed(41, 300, reset), 0, revalNow)
	if d.action != hintActionKeep || d.nextStatus(h) != "active" {
		t.Fatalf("decision = %+v status %q, want active again", d, d.nextStatus(h))
	}
	if !strings.Contains(d.reason, "resumed") {
		t.Fatalf("reason %q should say the executions resumed", d.reason)
	}
}

// A queryid absent from pg_stat_statements (a NULL sum(calls)) is dormant,
// never broken, and the checkpoint is left alone.
func TestDecideFromObservation_NotFoundIsDormant(t *testing.T) {
	reset := revalNow.Add(-72 * time.Hour)
	h := freshHint(ptrInt64(40), ptrTime(reset))
	d := decideFromObservation(h,
		pgssObservation{Found: false, ResetAt: ptrTime(reset)}, 0, revalNow)
	if d.action != hintActionDormant || d.nextStatus(h) != "dormant" {
		t.Fatalf("decision = %+v, want dormant", d)
	}
	if d.checkpoint != nil {
		t.Fatalf("checkpoint = %d, want unchanged (nil)", *d.checkpoint)
	}
	if !strings.Contains(d.reason, "absent") {
		t.Fatalf("reason %q should say the queryid is absent", d.reason)
	}
}

// After a reset the queryid may be absent: that is a reset (re-baselined
// to zero), and the hint keeps its status.
func TestDecideFromObservation_ResetAndAbsent(t *testing.T) {
	stored := revalNow.Add(-72 * time.Hour)
	h := freshHint(ptrInt64(40), ptrTime(stored))
	newer := revalNow.Add(-time.Minute)
	d := decideFromObservation(h,
		pgssObservation{Found: false, ResetAt: ptrTime(newer)}, 0, revalNow)
	if d.action != hintActionReset || d.nextStatus(h) != "active" {
		t.Fatalf("decision = %+v, want a reset that keeps active", d)
	}
	if d.checkpoint == nil || *d.checkpoint != 0 {
		t.Fatalf("checkpoint = %v, want 0", d.checkpoint)
	}
}

// First pass: no stored checkpoint and no stored stats_reset. The hint is
// active and both baselines are recorded.
func TestDecideFromObservation_FirstPassRecordsBaselines(t *testing.T) {
	reset := revalNow.Add(-72 * time.Hour)
	h := freshHint(nil, nil)
	d := decideFromObservation(h, observed(9, 300, reset), 0, revalNow)
	if d.action != hintActionKeep || d.checkpoint == nil || *d.checkpoint != 9 {
		t.Fatalf("decision = %+v, want active with checkpoint 9", d)
	}
	if d.resetAt == nil || !d.resetAt.Equal(reset) {
		t.Fatalf("resetAt = %v, want %v", d.resetAt, reset)
	}
}

// Age no longer retires: past HintRetirementDays the hint is stamped due for
// re-verification and keeps its status.
func TestDecideFromObservation_AgeMarksReverifyDue(t *testing.T) {
	reset := revalNow.Add(-72 * time.Hour)
	cutoff := 14 * 24 * time.Hour
	for _, tc := range []struct {
		name string
		age  time.Duration
		due  bool
	}{
		{"one minute before the cutoff", cutoff - time.Minute, false},
		{"one minute past the cutoff", cutoff + time.Minute, true},
		{"far past the cutoff", 100 * 24 * time.Hour, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := freshHint(ptrInt64(10), ptrTime(reset))
			h.CreatedAt = revalNow.Add(-tc.age)
			d := decideFromObservation(h, observed(20, 5, reset), cutoff, revalNow)
			if d.reverifyDue != tc.due {
				t.Fatalf("reverifyDue = %v, want %v", d.reverifyDue, tc.due)
			}
			if d.action != hintActionKeep || d.nextStatus(h) != "active" {
				t.Fatalf("decision = %+v, want the hint kept active", d)
			}
		})
	}
}

// HintRetirementDays=0 disables the age rule entirely.
func TestDecideFromObservation_ZeroCutoffNoAgeAction(t *testing.T) {
	reset := revalNow.Add(-72 * time.Hour)
	h := freshHint(ptrInt64(10), ptrTime(reset))
	h.CreatedAt = revalNow.Add(-1000 * 24 * time.Hour)
	d := decideFromObservation(h, observed(20, 5, reset), 0, revalNow)
	if d.reverifyDue {
		t.Fatal("cutoff 0 must not mark the hint due for re-verification")
	}
	if d.action != hintActionKeep {
		t.Fatalf("action = %v, want keep", d.action)
	}
}

// A hint already stamped due is not stamped again (the first stamp stays).
func TestDecideFromObservation_AlreadyDueNotRestamped(t *testing.T) {
	reset := revalNow.Add(-72 * time.Hour)
	h := freshHint(ptrInt64(10), ptrTime(reset))
	h.CreatedAt = revalNow.Add(-30 * 24 * time.Hour)
	h.ReverifyDue = ptrTime(revalNow.Add(-24 * time.Hour))
	d := decideFromObservation(h, observed(20, 5, reset), 14*24*time.Hour, revalNow)
	if d.reverifyDue {
		t.Fatal("an already-due hint must not be re-stamped")
	}
}

// A nil stats_reset (no reading) never counts as a reset by itself.
func TestDecideFromObservation_NilResetAt(t *testing.T) {
	h := freshHint(ptrInt64(10), ptrTime(revalNow.Add(-time.Hour)))
	d := decideFromObservation(h, pgssObservation{Calls: 12, Found: true}, 0, revalNow)
	if d.action != hintActionKeep || d.resetAt != nil {
		t.Fatalf("decision = %+v, want keep with no stats_reset to record", d)
	}
}

// Broken and error decisions map to their statuses; error leaves the
// status unchanged.
func TestHintDecision_NextStatus(t *testing.T) {
	h := hintRow{Status: "dormant"}
	for _, tc := range []struct {
		action hintAction
		want   string
	}{
		{hintActionKeep, "active"},
		{hintActionDormant, "dormant"},
		{hintActionReset, "dormant"},
		{hintActionBroken, "broken"},
		{hintActionError, "dormant"},
	} {
		if got := (hintDecision{action: tc.action}).nextStatus(h); got != tc.want {
			t.Errorf("action %v: next status = %q, want %q", tc.action, got, tc.want)
		}
	}
}
