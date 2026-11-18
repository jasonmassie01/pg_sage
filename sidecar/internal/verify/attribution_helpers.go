package verify

import (
	"math"
	"slices"
	"strings"
	"time"
)

// Helpers of the attribution (lab-01a): bucket cleaning, counter sums,
// reasons and ordered keys.

// cleanBuckets drops idle and malformed buckets (like Summarize) and merges
// buckets of one start (in UTC, so map keys compare), ordered by start.
func cleanBuckets(bs []Bucket) []Bucket {
	byStart := map[time.Time]Bucket{}
	for _, b := range bs {
		if b.Calls <= 0 || b.Rows < 0 || !finiteNonNeg(b.TotalMs) || !finiteNonNeg(b.Blocks) {
			continue
		}
		start := b.Start.UTC()
		m := byStart[start]
		m.Start = start
		m.Calls, m.TotalMs, m.Blocks = m.Calls+b.Calls, m.TotalMs+b.TotalMs, m.Blocks+b.Blocks
		m.Rows += b.Rows
		byStart[start] = m
	}
	out := make([]Bucket, 0, len(byStart))
	for _, b := range byStart {
		out = append(out, b)
	}
	slices.SortFunc(out, func(a, b Bucket) int { return a.Start.Compare(b.Start) })
	return out
}

func cleanSeries(s Series) Series {
	out := make(Series, len(s))
	for id, bs := range s {
		if c := cleanBuckets(bs); len(c) > 0 {
			out[id] = c
		}
	}
	return out
}

func finiteNonNeg(v float64) bool { return v >= 0 && !math.IsInf(v, 0) }

func callsOf(bs []Bucket) float64 {
	var c float64
	for _, b := range bs {
		if b.Calls > 0 {
			c += float64(b.Calls)
		}
	}
	return c
}

func blocksOf(s Series, weights map[int64]float64) float64 {
	var total float64
	for id := range weights {
		for _, b := range s[id] {
			total += b.Blocks
		}
	}
	return total
}

func joinReasons(reason string, notes []string) string {
	return strings.Join(nonEmpty(append([]string{reason}, notes...)), "; ")
}

func nonEmpty(ss []string) []string {
	return slices.DeleteFunc(slices.Clone(ss), func(s string) bool { return s == "" })
}

func sortedIDs(ids []int64) []int64 {
	out := slices.Clone(ids)
	slices.Sort(out)
	return slices.Compact(out)
}

func sortedKeys[V any](m map[int64]V) []int64 {
	out := make([]int64, 0, len(m))
	for id := range m {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

func logPct(v float64) float64 { return (math.Exp(v) - 1) * 100 }
