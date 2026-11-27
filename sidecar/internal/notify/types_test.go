package notify

import "testing"

// ux-02b: digest_ready is a notification event, so every channel kind gets
// the digest through the rules the owner already has.
// No concurrent access tests: the event tables are read-only package maps.

func TestDigestReady_IsAValidInfoEvent(t *testing.T) {
	if EventDigestReady != "digest_ready" {
		t.Fatalf("EventDigestReady = %q, want digest_ready", EventDigestReady)
	}
	if !ValidEventTypes[EventDigestReady] {
		t.Fatal("digest_ready is not in ValidEventTypes: a rule for it is refused")
	}
	if got := EventSeverity[EventDigestReady]; got != "info" {
		t.Fatalf("digest_ready severity = %q, want info", got)
	}
	if got := DefaultMinSeverity(EventDigestReady); got != "info" {
		t.Fatalf("DefaultMinSeverity(digest_ready) = %q, want info", got)
	}
}

// Boundary: an info rule fires, a warning or critical rule never can.
func TestDigestReady_RuleCanFireOnlyAtInfo(t *testing.T) {
	for sev, want := range map[string]bool{"info": true, "warning": false,
		"critical": false, "bogus": false} {
		if got := RuleCanFire(EventDigestReady, sev); got != want {
			t.Errorf("RuleCanFire(digest_ready, %q) = %v, want %v", sev, got, want)
		}
	}
}

// The digest is delivered at once, not queued: its row records each
// channel's real outcome, and it is sent once per schedule, not retried.
func TestDigestReady_IsNotQueueable(t *testing.T) {
	if queueable(Event{Type: EventDigestReady, Severity: "info"}) {
		t.Fatal("digest_ready may wait in the outbox; want delivery at once")
	}
	if !queueable(Event{Type: "action_executed", Severity: "info"}) {
		t.Fatal("action_executed is no longer queueable: the change leaked to other events")
	}
}
