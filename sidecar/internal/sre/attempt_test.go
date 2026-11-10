package sre

import (
	"encoding/json"
	"strings"
	"testing"
)

// sre-01 retry rule without a database: only the system's triggers retry,
// and only an expiry that is not a judgement (queue or stale evidence).
// No concurrent access test: the rule is a pure function of its inputs.

func TestRetriesAfter(t *testing.T) {
	expired := func(code string) Investigation {
		return Investigation{State: StateExpired, FailureCode: code, Attempt: 1}
	}
	system := StartRequest{IdempotencyKey: "incident:1"}
	trigger := StartRequest{IdempotencyKey: "incident:1", Actor: triggerActor}
	operator := StartRequest{IdempotencyKey: "incident:1", Actor: "user:7"}
	cases := []struct {
		name  string
		prior Investigation
		req   StartRequest
		want  bool
	}{
		{"queue expiry, system", expired(ReasonQueueExpired), system, true},
		{"queue expiry, trigger loop", expired(ReasonQueueExpired), trigger, true},
		{"stale evidence, trigger loop", expired(ReasonStaleEvidence), trigger, true},
		{"queue expiry, operator", expired(ReasonQueueExpired), operator, false},
		{"stale evidence, operator", expired(ReasonStaleEvidence), operator, false},
		{"expired, other code", expired("budget_exhausted"), system, false},
		{"expired, no code", expired(""), system, false},
		{"failed with a retry code", Investigation{State: StateFailed,
			FailureCode: ReasonQueueExpired}, system, false},
		{"concluded", Investigation{State: StateConcluded}, system, false},
		{"cancelled", Investigation{State: StateCancelled}, system, false},
		{"paused", Investigation{State: StatePaused}, system, false},
		{"queued", Investigation{State: StateQueued}, system, false},
		{"zero investigation", Investigation{}, system, false},
	}
	for _, c := range cases {
		if got := retriesAfter(c.prior, c.req); got != c.want {
			t.Errorf("%s: retriesAfter = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestStartRequestAutomatic(t *testing.T) {
	for actor, want := range map[string]bool{"": true, triggerActor: true,
		"user:1": false, "system": false, "Trigger": false, " trigger": false} {
		if got := (StartRequest{Actor: actor}).automatic(); got != want {
			t.Errorf("automatic(%q) = %v, want %v", actor, got, want)
		}
	}
}

// Exports and the API carry the attempt; a first attempt names no
// predecessor or successor.
func TestInvestigationJSON_Attempt(t *testing.T) {
	first, err := json.Marshal(Investigation{ID: "a", Attempt: 1})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(first), `"attempt":1`) ||
		strings.Contains(string(first), "supersedes") ||
		strings.Contains(string(first), "superseded_by") {
		t.Fatalf("attempt 1 JSON = %s", first)
	}
	second, err := json.Marshal(Investigation{ID: "b", Attempt: 2, Supersedes: "a",
		SupersededBy: "c"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{`"attempt":2`, `"supersedes":"a"`, `"superseded_by":"c"`} {
		if !strings.Contains(string(second), want) {
			t.Fatalf("attempt 2 JSON lacks %s: %s", want, second)
		}
	}
}

func TestWriteAttempt(t *testing.T) {
	cases := []struct {
		inv  Investigation
		want string
	}{
		{Investigation{}, ""},
		{Investigation{Attempt: 1}, ""},
		{Investigation{Attempt: 1, SupersededBy: "b"}, "- Attempt: 1 (superseded by b)\n"},
		{Investigation{Attempt: 2, Supersedes: "a"}, "- Attempt: 2 (supersedes a)\n"},
		{Investigation{Attempt: 3, Supersedes: "b", SupersededBy: "d"},
			"- Attempt: 3 (supersedes b, superseded by d)\n"},
	}
	for _, c := range cases {
		var b strings.Builder
		writeAttempt(&b, c.inv)
		if b.String() != c.want {
			t.Errorf("writeAttempt(%+v) = %q, want %q", c.inv, b.String(), c.want)
		}
	}
}

// Replay exports carry the contested run's attempt beside the case; the
// versioned case document itself is unchanged. A run read without the
// attempt column (zero) is the first attempt.
func TestBuildReplayCase_CarriesTheAttempt(t *testing.T) {
	for _, c := range []struct{ stored, want int }{{3, 3}, {MaxAttempts, MaxAttempts},
		{1, 1}, {0, 1}} {
		inv, ev := contestedIdle(t)
		inv.Attempt = c.stored
		exp, err := BuildReplayCase(inv, ev, refuted("ddl_lock_queue"),
			ReplayExportOptions{salt: fixedSalt})
		if err != nil || exp.Attempt != c.want {
			t.Fatalf("attempt %d: export attempt = %d (%v), want %d", c.stored,
				exp.Attempt, err, c.want)
		}
		var top, doc map[string]any
		if err := json.Unmarshal(mustJSON(t, exp), &top); err != nil {
			t.Fatalf("decode export: %v", err)
		}
		if top["attempt"] != float64(c.want) {
			t.Fatalf("attempt %d: export JSON attempt = %v", c.stored, top["attempt"])
		}
		if err := json.Unmarshal(mustJSON(t, exp.Case), &doc); err != nil {
			t.Fatalf("decode case: %v", err)
		}
		if _, ok := doc["attempt"]; ok {
			t.Fatalf("the versioned case document gained an attempt: %v", doc)
		}
	}
}
