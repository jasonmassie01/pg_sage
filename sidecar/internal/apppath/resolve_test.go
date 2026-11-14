package apppath

import (
	"context"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/catalogread"
)

// Invalid input: an empty name is refused before any session is used.
func TestResolveRefusesAnEmptyName(t *testing.T) {
	for _, name := range []string{"", "  \t"} {
		got, err := Resolve(context.Background(), nil, catalogread.Default(), name)
		if err == nil || got != "" || !strings.Contains(err.Error(), "no name") {
			t.Fatalf("Resolve(%q) = %q, %v; want the no-name error", name, got, err)
		}
	}
}

// Nil: no session is an error that names the relation, not a panic.
func TestResolveRefusesANilSession(t *testing.T) {
	got, err := Resolve(context.Background(), nil, catalogread.Default(), "orders")
	if err == nil || got != "" || !strings.Contains(err.Error(), "resolve orders") {
		t.Fatalf("Resolve(nil) = %q, %v; want an error naming orders", got, err)
	}
}
