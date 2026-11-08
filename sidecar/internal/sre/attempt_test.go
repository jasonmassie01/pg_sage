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
