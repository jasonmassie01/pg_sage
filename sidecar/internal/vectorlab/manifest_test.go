package vectorlab

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func validManifest() Manifest {
	return Manifest{
		Schema: "public", Table: "vectors", IDColumn: "id", VectorColumn: "embedding",
		Distance: "l2", K: 2, Repeats: 2, MinRecall: .95, MaxP95MS: 50,
		StatementTimeoutMS: 1000, TotalTimeoutMS: 10000,
		Variants: []Variant{{Name: "balanced", EFSearch: 100, IterativeScan: "strict_order"}},
		Queries: []Query{
			{ID: "a", Vector: []float64{1, 2}},
			{ID: "b", Vector: []float64{2, 3}},
			{ID: "c", Vector: []float64{3, 4}},
		},
	}
}

func TestManifestRejectsInvalidValues(t *testing.T) {
	cases := map[string]func(*Manifest){
		"empty":       func(m *Manifest) { *m = Manifest{} },
		"identifier":  func(m *Manifest) { m.Table = "bad\x00table" },
		"distance":    func(m *Manifest) { m.Distance = "<->; DROP TABLE x" },
		"zero budget": func(m *Manifest) { m.TotalTimeoutMS = 0 },
		"timeout":     func(m *Manifest) { m.StatementTimeoutMS = 30001 },
		"k":           func(m *Manifest) { m.K = 101 },
		"repeats":     func(m *Manifest) { m.Repeats = 0 },
		"recall":      func(m *Manifest) { m.MinRecall = math.NaN() },
		"latency":     func(m *Manifest) { m.MaxP95MS = math.Inf(1) },
		"dimensions":  func(m *Manifest) { m.Queries[1].Vector = []float64{1} },
		"nan vector":  func(m *Manifest) { m.Queries[0].Vector[0] = math.NaN() },
		"zero cosine": func(m *Manifest) {
			m.Distance = "cosine"
			m.Queries[0].Vector = []float64{0, 0}
		},
		"duplicate query":      func(m *Manifest) { m.Queries[1].ID = "a" },
		"insufficient queries": func(m *Manifest) { m.Queries = m.Queries[:2] },
		"no variants":          func(m *Manifest) { m.Variants = nil },
		"duplicate variant": func(m *Manifest) {
			m.Variants = append(m.Variants, m.Variants[0])
		},
		"ef":           func(m *Manifest) { m.Variants[0].EFSearch = 1001 },
		"mode":         func(m *Manifest) { m.Variants[0].IterativeScan = "unsafe" },
		"filter arity": func(m *Manifest) { m.FilterColumns = []string{"tenant"} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			m := validManifest()
			mutate(&m)
			if err := m.Validate(); err == nil {
				t.Fatal("invalid manifest accepted")
			}
		})
	}
}

func TestDecodeStrictAndBoundaryValid(t *testing.T) {
	m := validManifest()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(strings.NewReader(string(raw)))
	if err != nil || got.Table != m.Table || len(got.Queries) != 3 {
		t.Fatalf("decode = %#v, %v", got, err)
	}
	for _, raw := range []string{"", "null", "{}", string(raw) + " {}",
		strings.Replace(string(raw), "\"table\"", "\"typo\"", 1)} {
		if _, err := Decode(strings.NewReader(raw)); err == nil {
			t.Fatalf("accepted malformed input %q", raw)
		}
	}
	m.K, m.MinRecall, m.MaxP95MS = 100, 1, 60000
	m.Variants[0].EFSearch = 1000
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestBuildSQLQuotesIdentifiersAndBindsValues(t *testing.T) {
	m := validManifest()
	m.Table = `x"; DROP TABLE y; --`
	m.FilterColumns = []string{"tenant"}
	q := m.Queries[0]
	q.Filters = []string{"secret'; DELETE FROM vectors; --"}
	sql, args := buildSQL(m, "public", q, false)
	if !strings.Contains(sql, `"x""; DROP TABLE y; --"`) ||
		!strings.Contains(sql, `"tenant" = $3`) || strings.Contains(sql, "secret") {
		t.Fatalf("unsafe query %s", sql)
	}
	if len(args) != 3 || args[2] != q.Filters[0] || args[1] != m.K {
		t.Fatalf("incorrect bind args %#v", args)
	}
	exact, args := buildSQL(m, "public", q, true)
	if !strings.Contains(exact, "+ 0") || args[1] != m.K+1 {
		t.Fatalf("exact baseline lacks anti-ANN order/k+1: %s %#v", exact, args)
	}
}
