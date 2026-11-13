package vectorlab

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

type fakeSource struct {
	ann        []row
	exact      []row
	ties       []row
	indexes    []string
	failure    error
	tieFailure error
	seen       int
	tieCalls   int
	tieBounds  []float64
}

func (s *fakeSource) boundary(_ context.Context, _ Query, distance float64) ([]row, error) {
	s.tieCalls++
	s.tieBounds = append(s.tieBounds, distance)
	if s.tieFailure != nil {
		return nil, s.tieFailure
	}
	return s.ties, nil
}

func (s *fakeSource) observe(
	_ context.Context, _ Query, variant *Variant,
) (observation, error) {
	s.seen++
	if s.failure != nil {
		return observation{}, s.failure
	}
	if variant == nil {
		return observation{rows: s.exact, elapsed: 5 * time.Millisecond}, nil
	}
	return observation{rows: s.ann, elapsed: time.Millisecond, indexes: s.indexes}, nil
}

func healthySource() *fakeSource {
	return &fakeSource{
		exact:   []row{{"private-row-1", 1}, {"private-row-2", 2}, {"private-row-3", 3}},
		ann:     []row{{"private-row-1", 1}, {"private-row-2", 2}},
		indexes: []string{"idx_hnsw"},
	}
}

func TestEvidenceQualifiesAndRedacts(t *testing.T) {
	m := validManifest()
	s := healthySource()
	report, err := runExperiment(t.Context(), s, m)
	if err != nil || report.Recommendation != "balanced" || report.AutoApply {
		t.Fatalf("report = %#v, %v", report, err)
	}
	if report.Variants[0].MinRecall != 1 || report.Variants[0].P95MS != 1 ||
		report.ExactP95MS != 5 || s.seen != 12 {
		t.Fatalf("incorrect measurements %#v, observed=%d", report, s.seen)
	}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "private-row") || strings.Contains(string(raw), "vector\"") {
		t.Fatalf("report leaked workload: %s", raw)
	}
}

func TestEvidenceFailsClosed(t *testing.T) {
	cases := map[string]func(*fakeSource, *Manifest){
		"empty_ground_truth":   func(s *fakeSource, _ *Manifest) { s.exact = nil },
		"underfilled":          func(s *fakeSource, _ *Manifest) { s.ann = s.ann[:1] },
		"recall_below_target":  func(s *fakeSource, _ *Manifest) { s.ann[0].id = "wrong" },
		"hnsw_not_used":        func(s *fakeSource, _ *Manifest) { s.indexes = nil },
		"latency_above_target": func(_ *fakeSource, m *Manifest) { m.MaxP95MS = .5 },
	}
	for reason, mutate := range cases {
		t.Run(reason, func(t *testing.T) {
			s, m := healthySource(), validManifest()
			mutate(s, &m)
			r, err := runExperiment(t.Context(), s, m)
			if err != nil || r.Recommendation != "" ||
				!strings.Contains(strings.Join(r.Variants[0].Reasons, " "), reason) {
				t.Fatalf("failed to reject %s: %#v %v", reason, r, err)
			}
		})
	}
}

func TestEvidenceRejectsDuplicateAndInvalidRows(t *testing.T) {
	for _, invalid := range [][]row{
		{{"x", 1}, {"x", 2}}, {{"", 1}}, {{"x", 2}, {"y", 1}},
	} {
		s := healthySource()
		s.exact = invalid
		if _, err := runExperiment(t.Context(), s, validManifest()); err == nil {
			t.Fatalf("accepted invalid truth %#v", invalid)
		}
	}
	s := healthySource()
	s.ann = []row{{"x", 1}, {"x", 1}}
	if _, err := runExperiment(t.Context(), s, validManifest()); err == nil {
		t.Fatal("duplicate candidate IDs inflated recall")
	}
}

func TestEvidencePropagatesCancellationAndSourceFailure(t *testing.T) {
	s := healthySource()
	s.failure = context.Canceled
	if _, err := runExperiment(t.Context(), s, validManifest()); !errors.Is(err, context.Canceled) {
		t.Fatalf("lost cancellation: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	s = healthySource()
	if _, err := runExperiment(ctx, s, validManifest()); !errors.Is(err, context.Canceled) {
		t.Fatalf("ignored cancellation: %v", err)
	}
	if s.seen != 0 {
		t.Fatal("executed after cancellation")
	}
}

func TestRecallShortCorpusAndNearestRank(t *testing.T) {
	s := healthySource()
	s.exact, s.ann = s.exact[:1], s.ann[:1]
	r, err := runExperiment(t.Context(), s, validManifest())
	if err != nil || r.Recommendation == "" || r.Variants[0].MinRecall != 1 {
		t.Fatalf("short corpus mismeasured: %#v %v", r, err)
	}
	if got := percentile95([]float64{9, 2, 1, 5}); got != 9 {
		t.Fatalf("p95 = %v, want 9", got)
	}
}

// tiedSource has a tie at the k-th distance: rows 2 and 3 are equally near.
func tiedSource() *fakeSource {
	s := healthySource()
	s.exact = []row{{"row-1", 1}, {"row-2", 2}, {"row-3", 2}}
	s.ties = []row{{"row-1", 1}, {"row-2", 2}, {"row-3", 2}}
	s.ann = []row{{"row-1", 1}, {"row-3", 2}}
	return s
}

func TestEvidenceTieAtKAcceptsTheOtherTiedID(t *testing.T) {
	s := tiedSource()
	r, err := runExperiment(t.Context(), s, validManifest())
	if err != nil {
		t.Fatal(err)
	}
	v := r.Variants[0]
	if r.Recommendation != "balanced" || !v.Qualified || len(v.Reasons) != 0 ||
		v.MinRecall != 1 {
		t.Fatalf("tie at k disqualified a correct answer: %#v", r)
	}
	if !v.Queries[0].Ambiguous || r.AmbiguousBoundaryTies != 3 {
		t.Fatalf("tie must stay visible as information: %#v", r)
	}
	if s.tieCalls != 6 || s.tieBounds[0] != 2 {
		t.Fatalf("boundary fetch = %d calls, bounds %v; want 6 at distance 2",
			s.tieCalls, s.tieBounds)
	}
}

func TestEvidenceTieRecallCountsMissesAndCapsAtOne(t *testing.T) {
	s := tiedSource()
	s.ties = append(s.ties, row{"row-4", 2})
	s.ann = []row{{"row-4", 2}, {"far", 5}}
	r, err := runExperiment(t.Context(), s, validManifest())
	if err != nil || r.Recommendation != "" || r.Variants[0].MinRecall != .5 ||
		!contains(r.Variants[0].Reasons, "recall_below_target") ||
		contains(r.Variants[0].Reasons, "ambiguous_boundary_tie") {
		t.Fatalf("tie-set miss mismeasured: %#v %v", r, err)
	}
	s = tiedSource()
	s.ann = []row{{"row-1", 1}, {"row-2", 2}, {"row-3", 2}}
	r, err = runExperiment(t.Context(), s, validManifest())
	if err != nil || r.Variants[0].MinRecall != 1 || r.Recommendation != "balanced" {
		t.Fatalf("recall must cap at 1 when more tied IDs match than k: %#v %v", r, err)
	}
}

// Tied rows fill only the places left after the strictly nearer rows: two
// rows tied at the k-th distance cannot stand in for a missed nearest row.
func TestEvidenceTieRecallRequiresTheNearerRows(t *testing.T) {
	s := tiedSource()
	s.ann = []row{{"row-2", 2}, {"row-3", 2}}
	r, err := runExperiment(t.Context(), s, validManifest())
	v := r.Variants[0]
	if err != nil || r.Recommendation != "" || v.MinRecall != .5 ||
		!contains(v.Reasons, "recall_below_target") {
		t.Fatalf("missing the nearest row scored as full recall: %#v %v", r, err)
	}
}

func TestEvidenceTieBoundaryFailuresAbort(t *testing.T) {
	s := tiedSource()
	s.tieFailure = context.DeadlineExceeded
	if _, err := runExperiment(t.Context(), s, validManifest()); !errors.Is(err,
		context.DeadlineExceeded) || !strings.Contains(err.Error(), "boundary") {
		t.Fatalf("boundary failure lost: %v", err)
	}
	for name, ties := range map[string][]row{
		"short":     {{"row-1", 1}},
		"duplicate": {{"row-1", 1}, {"row-1", 2}, {"row-3", 2}},
		"beyond":    {{"row-1", 1}, {"row-2", 2}, {"row-9", 3}},
		"nearer":    {{"row-1", 1}, {"row-2", 1.5}, {"row-3", 2}},
	} {
		s := tiedSource()
		s.ties = ties
		if _, err := runExperiment(t.Context(), s, validManifest()); err == nil {
			t.Fatalf("%s boundary set accepted", name)
		}
	}
}

func TestEvidenceNoTieSkipsBoundaryFetch(t *testing.T) {
	s := healthySource()
	r, err := runExperiment(t.Context(), s, validManifest())
	if err != nil || s.tieCalls != 0 || r.AmbiguousBoundaryTies != 0 ||
		r.Variants[0].Queries[0].Ambiguous {
		t.Fatalf("untied truth fetched a boundary set: calls=%d %#v %v", s.tieCalls, r, err)
	}
}

func TestEvidenceRelaxedOrderIsResortedStrictIsNot(t *testing.T) {
	m := validManifest()
	m.Variants[0].IterativeScan = "relaxed_order"
	s := healthySource()
	s.ann = []row{{"private-row-2", 2}, {"private-row-1", 1}}
	r, err := runExperiment(t.Context(), s, m)
	if err != nil || r.Recommendation != "balanced" || r.Variants[0].MinRecall != 1 {
		t.Fatalf("relaxed out-of-order rows rejected: %#v %v", r, err)
	}
	for _, mode := range []string{"strict_order", "off"} {
		m.Variants[0].IterativeScan = mode
		s := healthySource()
		s.ann = []row{{"private-row-2", 2}, {"private-row-1", 1}}
		if _, err := runExperiment(t.Context(), s, m); err == nil ||
			!strings.Contains(err.Error(), "ordered") {
			t.Fatalf("%s out-of-order rows accepted: %v", mode, err)
		}
	}
	m.Variants[0].IterativeScan = "relaxed_order"
	s = healthySource()
	s.ann = []row{{"private-row-1", 2}, {"private-row-1", 1}}
	if _, err := runExperiment(t.Context(), s, m); err == nil {
		t.Fatal("relaxed duplicate IDs accepted")
	}
}

func TestEvidenceDefaultsKeepTheReportShape(t *testing.T) {
	r, err := runExperiment(t.Context(), healthySource(), validManifest())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}
	want := []string{"auto_apply", "exact_p95_ms", "format_version", "manifest_sha256",
		"postgres_version", "query_count", "recommendation", "repeats", "scope",
		"snapshot", "started_at", "variants", "vector_version"}
	if len(top) != len(want) || string(top["format_version"]) != "2" {
		t.Fatalf("report keys = %v, want %v with format_version 2", keys(top), want)
	}
	for _, key := range want {
		if _, ok := top[key]; !ok {
			t.Fatalf("report lost %s: %s", key, raw)
		}
	}
	settings, err := json.Marshal(r.Variants[0].Variant)
	if err != nil {
		t.Fatal(err)
	}
	if string(settings) != `{"name":"balanced","ef_search":100,"iterative_scan":"strict_order"}` {
		t.Fatalf("default variant settings changed shape: %s", settings)
	}
}

func keys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
