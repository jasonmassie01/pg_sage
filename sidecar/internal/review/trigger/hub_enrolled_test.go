package trigger

import (
	"errors"
	"strings"
	"testing"
)

// fleetpol-01b: discovery asks for a newly enrolled member's first review
// through the hub; with no queue registered yet the member is not ready.

func TestHub_EnrolledUnknownDatabase(t *testing.T) {
	h := NewHub()
	before := FireErrors()[KindEnrollment]
	err := h.Enrolled(t.Context(), "cluster-app-3", map[string]any{"set": "agent-created"})
	if !errors.Is(err, ErrUnknownDatabase) || !strings.Contains(err.Error(), "cluster-app-3") {
		t.Fatalf("err = %v, want ErrUnknownDatabase naming the member", err)
	}
	if got := FireErrors()[KindEnrollment]; got != before+1 {
		t.Fatalf("enrollment fire errors = %d, want %d", got, before+1)
	}
	if err := h.Enrolled(t.Context(), "", nil); !errors.Is(err, ErrUnknownDatabase) {
		t.Fatalf("empty name with no queue: %v", err)
	}
}

func TestHub_EnrolledStoreWithoutAPool(t *testing.T) {
	h := NewHub()
	h.Register("m", NewStore(nil), 1<<30)
	if err := h.Enrolled(t.Context(), "m", nil); !errors.Is(err, ErrNoStore) {
		t.Fatalf("err = %v, want ErrNoStore", err)
	}
}
