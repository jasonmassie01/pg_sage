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
	ann     []row
	exact   []row
	indexes []string
	failure error
	seen    int
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
		"empty_ground_truth": func(s *fakeSource, _ *Manifest) { s.exact = nil },
		"ambiguous_boundary_tie": func(s *fakeSource, _ *Manifest) {
			s.exact[2].distance = 2
		},
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
