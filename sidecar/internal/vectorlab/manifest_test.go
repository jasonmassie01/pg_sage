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
	sql, args := buildSQL(m, "public", "vector", q, false)
	if !strings.Contains(sql, `"x""; DROP TABLE y; --"`) ||
		!strings.Contains(sql, `"tenant" = $3`) || strings.Contains(sql, "secret") {
		t.Fatalf("unsafe query %s", sql)
	}
	if len(args) != 3 || args[2] != q.Filters[0] || args[1] != m.K {
		t.Fatalf("incorrect bind args %#v", args)
	}
	exact, args := buildSQL(m, "public", "vector", q, true)
	if !strings.Contains(exact, "+ 0") || args[1] != m.K+1 {
		t.Fatalf("exact baseline lacks anti-ANN order/k+1: %s %#v", exact, args)
	}
}

func intPtr(v int) *int { return &v }

func floatPtr(v float64) *float64 { return &v }

func dimensions(m *Manifest, n int) {
	for i := range m.Queries {
		m.Queries[i].Vector = make([]float64, n)
		m.Queries[i].Vector[0] = float64(i + 1)
	}
}

func TestManifestRejectsScanKnobsOutOfRange(t *testing.T) {
	cases := map[string]func(*Manifest){
		"max_scan_tuples zero": func(m *Manifest) { m.Variants[0].MaxScanTuples = intPtr(0) },
		"max_scan_tuples 2000000": func(m *Manifest) {
			m.Variants[0].MaxScanTuples = intPtr(2000000)
		},
		"max_scan_tuples 1000001": func(m *Manifest) {
			m.Variants[0].MaxScanTuples = intPtr(1000001)
		},
		"scan_mem_multiplier 1001": func(m *Manifest) {
			m.Variants[0].ScanMemMultiplier = floatPtr(1001)
		},
		"scan_mem_multiplier 0.5": func(m *Manifest) {
			m.Variants[0].ScanMemMultiplier = floatPtr(.5)
		},
		"scan_mem_multiplier NaN": func(m *Manifest) {
			m.Variants[0].ScanMemMultiplier = floatPtr(math.NaN())
		},
		"halfvec 4001 dimensions": func(m *Manifest) { dimensions(m, 4001) },
		"relaxed typo":            func(m *Manifest) { m.Variants[0].IterativeScan = "relaxed" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			m := validManifest()
			mutate(&m)
			err := m.Validate()
			field := strings.Fields(name)[0]
			if field == "halfvec" {
				field = "4000"
			}
			if field == "relaxed" {
				field = "relaxed_order"
			}
			if err == nil || !strings.Contains(err.Error(), field) {
				t.Fatalf("Validate() = %v, want an error naming %s", err, field)
			}
		})
	}
}

func TestManifestAcceptsScanKnobBoundaries(t *testing.T) {
	for _, v := range []Variant{
		{Name: "low", EFSearch: 1, IterativeScan: "relaxed_order",
			MaxScanTuples: intPtr(1), ScanMemMultiplier: floatPtr(1)},
		{Name: "high", EFSearch: 1000, IterativeScan: "relaxed_order",
			MaxScanTuples: intPtr(1000000), ScanMemMultiplier: floatPtr(1000)},
	} {
		m := validManifest()
		m.Variants = []Variant{v}
		dimensions(&m, 4000)
		if err := m.Validate(); err != nil {
			t.Fatalf("%s boundary rejected: %v", v.Name, err)
		}
	}
}

func TestDecodeDistinguishesAbsentFromZeroKnobs(t *testing.T) {
	raw, err := json.Marshal(validManifest())
	if err != nil {
		t.Fatal(err)
	}
	base := string(raw)
	with := func(fields string) string {
		return strings.Replace(base, `"iterative_scan":"strict_order"`,
			`"iterative_scan":"relaxed_order",`+fields, 1)
	}
	got, err := Decode(strings.NewReader(with(
		`"max_scan_tuples":200000,"scan_mem_multiplier":2.5`)))
	if err != nil || got.Variants[0].MaxScanTuples == nil ||
		*got.Variants[0].MaxScanTuples != 200000 ||
		*got.Variants[0].ScanMemMultiplier != 2.5 {
		t.Fatalf("decode knobs = %#v, %v", got.Variants, err)
	}
	if _, err := Decode(strings.NewReader(with(`"max_scan_tuples":0`))); err == nil {
		t.Fatal("explicit max_scan_tuples 0 accepted as the default")
	}
	got, err = Decode(strings.NewReader(base))
	if err != nil || got.Variants[0].MaxScanTuples != nil ||
		got.Variants[0].ScanMemMultiplier != nil {
		t.Fatalf("absent knobs must stay unset: %#v %v", got.Variants, err)
	}
}

func TestTargetDimensionsDependOnColumnType(t *testing.T) {
	for _, tc := range []struct {
		typ  string
		dims int
		ok   bool
	}{
		{"vector", 2000, true}, {"vector", 2001, false}, {"halfvec", 2001, true},
		{"halfvec", 4000, true}, {"halfvec", 4001, false}, {"bit", 2, false},
		{"", 2, false}, {"vector", 0, false},
	} {
		err := checkTargetDimensions(tc.typ, tc.dims)
		if (err == nil) != tc.ok {
			t.Fatalf("checkTargetDimensions(%q, %d) = %v, want ok=%v",
				tc.typ, tc.dims, err, tc.ok)
		}
	}
	if err := checkTargetDimensions("vector", 3072); err == nil ||
		!strings.Contains(err.Error(), "halfvec") {
		t.Fatalf("vector over 2000 must point at halfvec: %v", err)
	}
}

func TestBuildSQLCastsToTheColumnType(t *testing.T) {
	m := validManifest()
	q := m.Queries[0]
	sql, _ := buildSQL(m, "ext", "halfvec", q, false)
	if !strings.Contains(sql, `$1::"ext"."halfvec"`) || strings.Contains(sql, ".vector") {
		t.Fatalf("halfvec query not cast to halfvec: %s", sql)
	}
	sql, _ = buildSQL(m, "ext", "vector", q, false)
	if !strings.Contains(sql, `$1::"ext"."vector"`) {
		t.Fatalf("vector query not cast to vector: %s", sql)
	}
}

func TestBuildBoundarySQLBindsTheDistance(t *testing.T) {
	m := validManifest()
	m.FilterColumns = []string{"tenant"}
	q := m.Queries[0]
	q.Filters = []string{"7"}
	sql, args := buildBoundarySQL(m, "public", "vector", q, 2.5)
	if !strings.Contains(sql, "<= $4") || !strings.Contains(sql, "+ 0") ||
		!strings.Contains(sql, `"tenant" = $3`) || strings.Contains(sql, "2.5") {
		t.Fatalf("boundary query must bind the k-th distance and order exactly: %s", sql)
	}
	if len(args) != 4 || args[1] != 2*m.K || args[2] != "7" || args[3] != 2.5 {
		t.Fatalf("boundary args = %#v, want limit 2k and bound distance last", args)
	}
}
