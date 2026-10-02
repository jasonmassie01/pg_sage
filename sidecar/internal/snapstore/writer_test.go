package snapstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

// withBase installs a committed keyframe for category, as Persist would.
func withBase(t *testing.T, w *Writer, category string, id int64, at time.Time, doc []byte) {
	t.Helper()
	c, err := parseCatalog(doc, keyFields[category])
	if err != nil {
		t.Fatalf("parseCatalog: %v", err)
	}
	w.bases[category] = &keyframe{id: id, at: at, size: len(doc), list: c}
}

// State transition: the first document of a delta category is a full row
// that becomes the base; the next one is a delta against it.
func TestPlan_FirstDocumentIsKeyframeThenDelta(t *testing.T) {
	w := NewWriter()
	doc := list(idx("a", 1), idx("b", 2))
	p, err := w.plan("indexes", doc, t0)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if p.baseID != 0 || string(p.payload) != string(doc) || p.next == nil {
		t.Fatalf("first plan = %+v, want a full keyframe row", p)
	}
	withBase(t, w, "indexes", 42, t0, doc)
	p, err = w.plan("indexes", list(idx("a", 1), idx("b", 3)), t0.Add(time.Minute))
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if p.baseID != 42 || p.next != nil || !strings.Contains(string(p.payload), `"u"`) {
		t.Fatalf("second plan = %+v (%s), want a delta on base 42", p, p.payload)
	}
}

// Boundary: a keyframe serves for keyframeMaxAge; at exactly that age the
// next document starts a new keyframe.
func TestPlan_KeyframeAgeBoundary(t *testing.T) {
	w := NewWriter()
	doc := list(idx("a", 1))
	withBase(t, w, "indexes", 7, t0, doc)
	p, err := w.plan("indexes", doc, t0.Add(keyframeMaxAge-time.Nanosecond))
	if err != nil || p.baseID != 7 {
		t.Fatalf("just under max age: plan = %+v (%v), want a delta", p, err)
	}
	p, err = w.plan("indexes", doc, t0.Add(keyframeMaxAge))
	if err != nil || p.baseID != 0 || p.next == nil {
		t.Fatalf("at max age: plan = %+v (%v), want a new keyframe", p, err)
	}
}

// A document collected before the base (a clock step back) never uses it.
func TestPlan_DocumentOlderThanBaseIsKeyframe(t *testing.T) {
	w := NewWriter()
	doc := list(idx("a", 1))
	withBase(t, w, "indexes", 7, t0, doc)
	p, err := w.plan("indexes", doc, t0.Add(-time.Second))
	if err != nil || p.baseID != 0 || p.next == nil {
		t.Fatalf("plan = %+v (%v), want a keyframe", p, err)
	}
}

// Boundary: a delta larger than half its base is not worth it; a full row
// (and new base) is written instead.
func TestPlan_LargeDeltaBecomesKeyframe(t *testing.T) {
	w := NewWriter()
	var before, after []string
	for i := 0; i < 20; i++ {
		before = append(before, idx(fmt.Sprintf("i%02d", i), 0))
		after = append(after, idx(fmt.Sprintf("j%02d", i), 0)) // all replaced
	}
	withBase(t, w, "indexes", 9, t0, list(before...))
	p, err := w.plan("indexes", list(after...), t0.Add(time.Minute))
	if err != nil || p.baseID != 0 || p.next == nil {
		t.Fatalf("plan = %+v (%v), want a keyframe", p, err)
	}
	// One counter change on the same base stays a delta.
	changed := append([]string{idx("i00", 5)}, before[1:]...)
	p, err = w.plan("indexes", list(changed...), t0.Add(time.Minute))
	if err != nil || p.baseID != 9 || len(p.payload) > len(list(before...))/maxDeltaShare {
		t.Fatalf("plan = %+v (%v), want a small delta", p, err)
	}
}

// Categories without a stable element identity (system, locks, io,
// config_data, replication) are always stored in full and never become a
// base.
func TestPlan_NonCatalogCategoryIsAlwaysFull(t *testing.T) {
	w := NewWriter()
	doc := []byte(`{"db_size_bytes":1}`)
	for i := 0; i < 2; i++ {
		p, err := w.plan("system", doc, t0.Add(time.Duration(i)*time.Minute))
		if err != nil || p.baseID != 0 || p.next != nil || string(p.payload) != string(doc) {
			t.Fatalf("plan %d = %+v (%v), want a plain full row", i, p, err)
		}
	}
	if len(w.bases) != 0 {
		t.Fatalf("bases = %v, want none", w.bases)
	}
}

// A document a delta cannot express is stored in full and is not a base:
// the next document is stored in full too (never a delta on a bad base).
func TestPlan_NotEncodableDocumentIsFullAndNoBase(t *testing.T) {
	w := NewWriter()
	withBase(t, w, "indexes", 3, t0, list(idx("a", 1)))
	dup := list(idx("a", 1), idx("a", 2))
	p, err := w.plan("indexes", dup, t0.Add(time.Minute))
	if err != nil || p.baseID != 0 || string(p.payload) != string(dup) {
		t.Fatalf("plan = %+v (%v), want a full row", p, err)
	}
	if p.next == nil || p.next.list != nil {
		t.Fatalf("next = %+v, want the base cleared (no usable list)", p.next)
	}
	null := []byte(`null`)
	p, err = w.plan("indexes", null, t0.Add(time.Minute))
	if err != nil || p.baseID != 0 || string(p.payload) != "null" {
		t.Fatalf("null plan = %+v (%v), want a full null row", p, err)
	}
}

// Invalid input is refused with distinguishable errors.
func TestPlan_InvalidInput(t *testing.T) {
	w := NewWriter()
	if _, err := w.plan("", []byte(`[]`), t0); !errors.Is(err, ErrInvalidRow) ||
		!strings.Contains(err.Error(), "category") {
		t.Errorf("empty category: err = %v", err)
	}
	if _, err := w.plan("indexes", nil, t0); !errors.Is(err, ErrInvalidRow) ||
		!strings.Contains(err.Error(), "indexes") {
		t.Errorf("empty data: err = %v", err)
	}
	if _, err := w.plan("indexes", []byte(`[]`), time.Time{}); !errors.Is(err, ErrInvalidRow) ||
		!strings.Contains(err.Error(), "time") {
		t.Errorf("zero time: err = %v", err)
	}
}

// Error propagation: Persist without a database fails before any work.
func TestPersist_NilDatabase(t *testing.T) {
	err := NewWriter().Persist(context.Background(), nil, t0,
		[]Row{{Category: "indexes", Data: []byte(`[]`)}})
	if err == nil || !strings.Contains(err.Error(), "database") {
		t.Fatalf("err = %v, want a missing-database error", err)
	}
}

// Nil/empty: no rows is a no-op that needs no transaction.
func TestPersist_NoRowsIsNoOp(t *testing.T) {
	if err := NewWriter().Persist(context.Background(), nil, t0, nil); err != nil {
		t.Fatalf("err = %v, want nil for no rows", err)
	}
}

// The SQL helpers name the accessor function and the alias.
func TestSQLHelpers(t *testing.T) {
	if got := DataSQL("s"); got != "sage.snapshot_data(s.data, s.base_id)" {
		t.Fatalf("DataSQL = %q", got)
	}
	if got := DataSQL(""); got != "sage.snapshot_data(data, base_id)" {
		t.Fatalf("DataSQL(\"\") = %q", got)
	}
	got := NonEmptySQL("s")
	for _, part := range []string{"s.base_id IS NULL", "pg_column_size(s.data) > 12",
		"(s.data->>'n')::int > 0"} {
		if !strings.Contains(got, part) {
			t.Fatalf("NonEmptySQL = %q, missing %q", got, part)
		}
	}
}
