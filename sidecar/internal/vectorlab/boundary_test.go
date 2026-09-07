package vectorlab

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestResourceBoundariesAndReadFailure(t *testing.T) {
	m := validManifest()
	m.Queries = make([]Query, 200)
	for i := range m.Queries {
		m.Queries[i] = Query{ID: strings.Repeat("a", i+1), Vector: []float64{1}}
	}
	m.Variants = make([]Variant, 16)
	m.Repeats = 20
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "10000") {
		t.Fatalf("unbounded workload accepted: %v", err)
	}
	if _, err := Decode(strings.NewReader(strings.Repeat(" ", 16*1024*1024+1))); err == nil {
		t.Fatal("oversized input accepted")
	}
	if _, err := Decode(failingReader{}); err == nil {
		t.Fatal("reader error swallowed")
	}
	if _, err := Run(t.Context(), nil, validManifest()); err == nil {
		t.Fatal("nil pool accepted")
	}
	if percentile95(nil) != 0 {
		t.Fatal("empty percentile must be zero")
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }

func TestSafeErrorsAndVersionCapabilities(t *testing.T) {
	for _, version := range []string{"0.8.0", "0.8.6", "1.0.0"} {
		if !supportsIterative(version) {
			t.Fatalf("supported version rejected: %s", version)
		}
	}
	for _, version := range []string{"", "x", "0.7.4"} {
		if supportsIterative(version) {
			t.Fatalf("unsupported version accepted: %s", version)
		}
	}
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		if !errors.Is(safeError("search", cause), cause) {
			t.Fatal("lost cancellation identity")
		}
	}
	err := safeError("search", &pgconn.PgError{Code: "42501", Message: "private-input"})
	if !strings.Contains(err.Error(), "42501") || strings.Contains(err.Error(), "private-input") {
		t.Fatalf("incorrect safe error: %v", err)
	}
}

func TestPostgresMissingExtension(t *testing.T) {
	p := testPool(t)
	if _, err := p.Exec(t.Context(), "DROP EXTENSION vector"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := p.Exec(context.Background(), "CREATE EXTENSION vector"); err != nil {
			t.Error(err)
		}
	})
	if _, err := Run(t.Context(), p, validManifest()); err == nil ||
		!strings.Contains(err.Error(), "pgvector extension is required") {
		t.Fatalf("missing extension error: %v", err)
	}
}
