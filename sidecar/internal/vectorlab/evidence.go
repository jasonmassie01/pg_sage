package vectorlab

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
)

type source interface {
	observe(context.Context, Query, *Variant) (observation, error)
}

type experiment struct {
	report         Report
	latencies      [][]float64
	exactLatencies []float64
}

func runExperiment(ctx context.Context, s source, m Manifest) (Report, error) {
	if err := m.Validate(); err != nil {
		return Report{}, err
	}
	x := newExperiment(m)
	for qi, q := range m.Queries {
		for repeat := range m.Repeats {
			if err := ctx.Err(); err != nil {
				return Report{}, err
			}
			truth, err := s.observe(ctx, q, nil)
			if err != nil {
				return Report{}, fmt.Errorf("exact query %d: %w", qi, err)
			}
			if err := checkRows(truth.rows); err != nil {
				return Report{}, err
			}
			x.exactLatencies = append(x.exactLatencies, milliseconds(truth))
			for offset := range m.Variants {
				vi := (qi + repeat + offset) % len(m.Variants)
				if err := ctx.Err(); err != nil {
					return Report{}, err
				}
				observed, err := s.observe(ctx, q, &m.Variants[vi])
				if err != nil {
					return Report{}, fmt.Errorf("candidate query %d: %w", qi, err)
				}
				if err := checkRows(observed.rows); err != nil {
					return Report{}, err
				}
				x.record(m, qi, vi, truth, observed)
			}
		}
	}
	return x.finish(m), nil
}

func newExperiment(m Manifest) *experiment {
	raw, _ := json.Marshal(m) // Validation precedes this; no nonfinite numbers remain.
	sum := sha256.Sum256(raw)
	x := &experiment{
		report: Report{FormatVersion: 1, ManifestSHA256: hex.EncodeToString(sum[:]),
			QueryCount: len(m.Queries), Repeats: m.Repeats,
			Scope: "advisory warm-cache experiment; sample evidence, not production certification"},
		latencies: make([][]float64, len(m.Variants)),
	}
	for _, v := range m.Variants {
		result := VariantResult{Variant: v, MinRecall: 1, Reasons: []string{}}
		for _, q := range m.Queries {
			result.Queries = append(result.Queries, QueryResult{QueryID: q.ID, MinRecall: 1})
		}
		x.report.Variants = append(x.report.Variants, result)
	}
	return x
}

func (x *experiment) record(m Manifest, qi, vi int, truth, observed observation) {
	result := &x.report.Variants[vi]
	q := &result.Queries[qi]
	count := min(m.K, len(truth.rows))
	q.TruthCount = count
	q.Ambiguous = len(truth.rows) > m.K &&
		truth.rows[m.K-1].distance == truth.rows[m.K].distance
	if count == 0 {
		addReason(result, "empty_ground_truth")
	}
	if q.Ambiguous {
		addReason(result, "ambiguous_boundary_tie")
	}
	if len(observed.rows) < count {
		q.Underfilled++
		addReason(result, "underfilled")
	}
	recall := recallAtK(truth.rows[:count], observed.rows)
	q.MinRecall = min(q.MinRecall, recall)
	result.MinRecall = min(result.MinRecall, recall)
	if recall < m.MinRecall {
		addReason(result, "recall_below_target")
	}
	if len(observed.indexes) == 0 {
		addReason(result, "hnsw_not_used")
	}
	for _, index := range observed.indexes {
		if !contains(q.IndexNames, index) {
			q.IndexNames = append(q.IndexNames, index)
		}
	}
	sort.Strings(q.IndexNames)
	x.latencies[vi] = append(x.latencies[vi], milliseconds(observed))
}

func (x *experiment) finish(m Manifest) Report {
	x.report.ExactP95MS = percentile95(x.exactLatencies)
	best := -1
	for i := range x.report.Variants {
		v := &x.report.Variants[i]
		v.P95MS = percentile95(x.latencies[i])
		if v.P95MS > m.MaxP95MS {
			addReason(v, "latency_above_target")
		}
		v.Qualified = len(v.Reasons) == 0
		sort.Strings(v.Reasons)
		if v.Qualified && (best < 0 || v.P95MS < x.report.Variants[best].P95MS ||
			(v.P95MS == x.report.Variants[best].P95MS &&
				v.Variant.Name < x.report.Variants[best].Variant.Name)) {
			best = i
		}
	}
	if best >= 0 {
		x.report.Recommendation = x.report.Variants[best].Variant.Name
	}
	return x.report
}

func checkRows(rows []row) error {
	seen := make(map[string]bool)
	for i, r := range rows {
		if r.id == "" || seen[r.id] || !finite(r.distance) ||
			(i > 0 && r.distance < rows[i-1].distance) {
			return errors.New("invalid result: require unique nonempty IDs and finite ordered distances")
		}
		seen[r.id] = true
	}
	return nil
}

func recallAtK(truth, candidate []row) float64 {
	if len(truth) == 0 {
		return 0
	}
	ids := make(map[string]bool, len(truth))
	for _, r := range truth {
		ids[r.id] = true
	}
	matches := 0
	for _, r := range candidate {
		if ids[r.id] {
			matches++
			delete(ids, r.id)
		}
	}
	return float64(matches) / float64(len(truth))
}

func percentile95(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	return sorted[int(math.Ceil(float64(len(sorted))*.95))-1]
}

func addReason(result *VariantResult, reason string) {
	if !contains(result.Reasons, reason) {
		result.Reasons = append(result.Reasons, reason)
	}
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func milliseconds(o observation) float64 { return float64(o.elapsed) / 1e6 }
