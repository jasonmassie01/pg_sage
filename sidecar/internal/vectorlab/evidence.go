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
	boundary(context.Context, Query, float64) ([]row, error)
}

type experiment struct {
	report         Report
	latencies      [][]float64
	exactLatencies []float64
	tiedQueries    map[int]bool
}

// truth is the exact answer for one query: the k+1 nearest rows and, when the
// k-th distance (bound) is tied, every row at or nearer than it (at most 2k).
type truth struct {
	observation
	tieSet []row
	bound  float64
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
			exact, err := measureTruth(ctx, s, m, q)
			if err != nil {
				return Report{}, fmt.Errorf("exact query %d: %w", qi, err)
			}
			x.exactLatencies = append(x.exactLatencies, milliseconds(exact.observation))
			if exact.tieSet != nil {
				x.tiedQueries[qi] = true
			}
			for offset := range m.Variants {
				vi := (qi + repeat + offset) % len(m.Variants)
				if err := ctx.Err(); err != nil {
					return Report{}, err
				}
				observed, err := measureCandidate(ctx, s, q, &m.Variants[vi])
				if err != nil {
					return Report{}, fmt.Errorf("candidate query %d: %w", qi, err)
				}
				x.record(m, qi, vi, exact, observed)
			}
		}
	}
	return x.finish(m), nil
}

func measureTruth(ctx context.Context, s source, m Manifest, q Query) (truth, error) {
	exact, err := s.observe(ctx, q, nil)
	if err != nil {
		return truth{}, err
	}
	if err := checkRows(exact.rows); err != nil {
		return truth{}, err
	}
	result := truth{observation: exact}
	if len(exact.rows) <= m.K || exact.rows[m.K-1].distance != exact.rows[m.K].distance {
		return result, nil
	}
	result.bound = exact.rows[m.K-1].distance
	result.tieSet, err = s.boundary(ctx, q, result.bound)
	if err != nil {
		return truth{}, fmt.Errorf("boundary tie rows: %w", err)
	}
	if err := checkTieSet(result.tieSet, m.K, result.bound); err != nil {
		return truth{}, err
	}
	return result, nil
}

// measureCandidate runs one variant. relaxed_order may return rows out of
// distance order by design, so they are re-sorted before the order check.
func measureCandidate(
	ctx context.Context, s source, q Query, v *Variant,
) (observation, error) {
	observed, err := s.observe(ctx, q, v)
	if err != nil {
		return observation{}, err
	}
	if v.IterativeScan == "relaxed_order" {
		observed.rows = append([]row(nil), observed.rows...)
		sort.SliceStable(observed.rows, func(i, j int) bool {
			return observed.rows[i].distance < observed.rows[j].distance
		})
	}
	return observed, checkRows(observed.rows)
}

func newExperiment(m Manifest) *experiment {
	raw, _ := json.Marshal(m) // Validation precedes this; no nonfinite numbers remain.
	sum := sha256.Sum256(raw)
	x := &experiment{
		report: Report{FormatVersion: 2, ManifestSHA256: hex.EncodeToString(sum[:]),
			QueryCount: len(m.Queries), Repeats: m.Repeats,
			Scope: "advisory warm-cache experiment; sample evidence, not production certification"},
		latencies:   make([][]float64, len(m.Variants)),
		tiedQueries: make(map[int]bool),
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

func (x *experiment) record(m Manifest, qi, vi int, exact truth, observed observation) {
	result := &x.report.Variants[vi]
	q := &result.Queries[qi]
	count := min(m.K, len(exact.rows))
	q.TruthCount = count
	q.Ambiguous = q.Ambiguous || exact.tieSet != nil
	if count == 0 {
		addReason(result, "empty_ground_truth")
	}
	if len(observed.rows) < count {
		q.Underfilled++
		addReason(result, "underfilled")
	}
	recall := recallAgainst(exact.rows[:count], observed.rows, count)
	if exact.tieSet != nil {
		recall = tieRecall(exact.tieSet, observed.rows, m.K, exact.bound)
	}
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
	x.report.AmbiguousBoundaryTies = len(x.tiedQueries)
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

// checkTieSet verifies the boundary rows agree with the exact baseline: valid
// rows, none beyond the k-th distance, fewer than k strictly nearer, and at
// least the k+1 already seen.
func checkTieSet(rows []row, k int, bound float64) error {
	if err := checkRows(rows); err != nil {
		return err
	}
	if len(rows) <= k || rows[len(rows)-1].distance > bound || rows[k-1].distance != bound {
		return errors.New("invalid boundary tie rows: disagree with the exact baseline")
	}
	return nil
}

// tieRecall scores a candidate against a tie at the k-th distance. Every row
// strictly nearer than bound belongs in any correct top k; the tied rows may
// fill only the places left after them. So a tied row can stand in for
// another tied row, never for a missed nearer one.
func tieRecall(ties, candidate []row, k int, bound float64) float64 {
	nearer, tied := make(map[string]bool), make(map[string]bool)
	for _, r := range ties {
		if r.distance < bound {
			nearer[r.id] = true
		} else {
			tied[r.id] = true
		}
	}
	nearerHits, tiedHits := 0, 0
	for _, r := range candidate {
		switch {
		case nearer[r.id]:
			nearerHits++
		case tied[r.id]:
			tiedHits++
		}
	}
	matches := nearerHits + min(tiedHits, k-len(nearer))
	return min(1, float64(matches)/float64(k))
}

// recallAgainst is the share of k answers found in the truth rows.
func recallAgainst(accepted, candidate []row, k int) float64 {
	if k == 0 {
		return 0
	}
	ids := make(map[string]bool, len(accepted))
	for _, r := range accepted {
		ids[r.id] = true
	}
	matches := 0
	for _, r := range candidate {
		if ids[r.id] {
			matches++
			delete(ids, r.id)
		}
	}
	return min(1, float64(matches)/float64(k))
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
