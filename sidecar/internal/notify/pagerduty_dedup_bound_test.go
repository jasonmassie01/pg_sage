package notify

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// No concurrent access tests: BoundPagerDutyDedupKey is a pure function of its
// input and the payload builder shares no state.

// PagerDuty rejects a dedup_key over 255 characters, so a critical
// finding on a long object identifier would never page. The key stays
// within the limit and the trigger and its resolve still share it.
func TestPagerDutyLongFindingKeyBoundedAndShared(t *testing.T) {
	ch := Channel{Config: map[string]string{"dedup_key_prefix": "prod"}}
	object := "public." + strings.Repeat("o", 300)
	trigger := FindingCriticalEvent("t", "d", "orders", "table_bloat", object)
	keyT := pdDedupKey(t, ch, trigger)
	keyR := pdDedupKey(t, ch, ResolvedEvent(trigger))
	if len(keyT) != pdMaxDedupKey {
		t.Fatalf("key length = %d, want %d: %q", len(keyT), pdMaxDedupKey, keyT)
	}
	if keyT != keyR {
		t.Fatalf("trigger key %q != resolve key %q", keyT, keyR)
	}
	if !strings.HasPrefix(keyT, "prod:finding_critical:orders:table_bloat:public.ooo") {
		t.Fatalf("bounded key lost its readable head: %q", keyT)
	}
}

// Two long identities that share their first 255 characters must still
// be two PagerDuty incidents.
func TestPagerDutyLongKeysSharingHeadStayDistinct(t *testing.T) {
	ch := Channel{Config: map[string]string{}}
	head := "public." + strings.Repeat("x", 300)
	a := pdDedupKey(t, ch, FindingCriticalEvent("t", "d", "db", "c", head+"_a"))
	b := pdDedupKey(t, ch, FindingCriticalEvent("t", "d", "db", "c", head+"_b"))
	if a == b {
		t.Fatalf("distinct long identities collapsed into one key %q", a)
	}
}

// Boundary: 255 characters is within the limit and kept verbatim; 256 is
// bounded.
func TestBoundPDDedupKeyBoundary(t *testing.T) {
	at := strings.Repeat("k", pdMaxDedupKey)
	if got := BoundPagerDutyDedupKey(at); got != at {
		t.Fatalf("255-char key changed: %q", got)
	}
	over := at + "z"
	got := BoundPagerDutyDedupKey(over)
	if len(got) != pdMaxDedupKey || got == over[:pdMaxDedupKey] {
		t.Fatalf("256-char key not bounded with a hash: len %d %q", len(got), got)
	}
	if BoundPagerDutyDedupKey(over) != got {
		t.Fatal("bounding is not deterministic")
	}
}

// Nil/empty: an empty key stays empty (the generic key is computed
// before bounding, so this only guards the helper).
func TestBoundPDDedupKeyEmpty(t *testing.T) {
	if got := BoundPagerDutyDedupKey(""); got != "" {
		t.Fatalf("empty key = %q", got)
	}
}

// Invalid input: a multi-byte identifier is cut on a rune boundary, so
// the JSON payload never carries a broken UTF-8 sequence.
func TestBoundPDDedupKeyKeepsValidUTF8(t *testing.T) {
	key := strings.Repeat("a", 240) + strings.Repeat("é", 40)
	got := BoundPagerDutyDedupKey(key)
	if !utf8.ValidString(got) {
		t.Fatalf("bounded key is not valid UTF-8: %q", got)
	}
	if len(got) > pdMaxDedupKey {
		t.Fatalf("bounded key too long: %d", len(got))
	}
}
