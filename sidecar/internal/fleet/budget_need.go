package fleet

import (
	"math"
	"sort"
)

// Fleet budget split modes.
const (
	SplitEven = "even"
	SplitNeed = "need"
)

// SetSplit selects how TotalDaily is split: SplitNeed shares it by measured
// need, each database between floorPct and ceilingPct of the even share;
// anything else splits evenly. It rebalances at once.
func (b *FleetBudget) SetSplit(mode string, floorPct, ceilingPct int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if mode != SplitNeed {
		mode = SplitEven
	}
	b.split, b.floorPct, b.ceilingPct = mode, floorPct, ceilingPct
	b.rebalanceLocked()
}

// Split is the current split mode.
func (b *FleetBudget) Split() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.split == "" {
		return SplitEven
	}
	return b.split
}

// SetNeeds records each registered database's measured need weight and
// rebalances. Needs of unregistered databases are ignored: a need never
// registers a database.
func (b *FleetBudget) SetNeeds(weights map[string]float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for name, w := range weights {
		if db := b.perDB[name]; db != nil {
			db.weight = w
		}
	}
	b.rebalanceLocked()
}

// AllocateByNeed splits total across the weighted databases: every
// database gets at least floorPct and at most ceilingPct of the even share
// (total/n), the rest follows the weights, and capacity freed by ceilings
// is passed on. Non-positive weights count as 1. Allocations are integers
// and their sum never exceeds total.
func AllocateByNeed(total int, weights map[string]float64, floorPct,
	ceilingPct int) map[string]int {
	out := make(map[string]int, len(weights))
	if len(weights) == 0 {
		return out
	}
	if total < 0 {
		total = 0
	}
	floorPct = clampInt(floorPct, 0, 100)
	if ceilingPct < 100 {
		ceilingPct = 100
	}
	even := float64(total) / float64(len(weights))
	floor, ceiling := even*float64(floorPct)/100, even*float64(ceilingPct)/100
	shares := waterFill(float64(total), positiveWeights(weights), floor, ceiling)
	return roundDown(shares, total)
}

func positiveWeights(weights map[string]float64) map[string]float64 {
	out := make(map[string]float64, len(weights))
	for name, w := range weights {
		if w <= 0 || math.IsNaN(w) || math.IsInf(w, 0) {
			w = 1
		}
		out[name] = w
	}
	return out
}

// waterFill gives every database its floor, then shares the rest by
// weight among databases below the ceiling, repeating as ceilings bind.
func waterFill(total float64, weights map[string]float64, floor,
	ceiling float64) map[string]float64 {
	shares := make(map[string]float64, len(weights))
	open := make(map[string]bool, len(weights))
	for name := range weights {
		shares[name] = floor
		open[name] = true
	}
	remaining := total - floor*float64(len(weights))
	for remaining > 1e-9 && len(open) > 0 {
		sumW := 0.0
		for name := range open {
			sumW += weights[name]
		}
		spent := 0.0
		for name := range open {
			give := remaining * weights[name] / sumW
			if shares[name]+give >= ceiling {
				give = ceiling - shares[name]
				delete(open, name)
			}
			shares[name] += give
			spent += give
		}
		remaining -= spent
		if spent <= 1e-9 {
			break
		}
	}
	return shares
}

// roundDown floors the shares and hands the leftover units out by largest
// fractional part, never exceeding total.
func roundDown(shares map[string]float64, total int) map[string]int {
	names := make([]string, 0, len(shares))
	out := make(map[string]int, len(shares))
	used := 0
	for name, s := range shares {
		out[name] = int(math.Floor(s))
		used += out[name]
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		fi := shares[names[i]] - math.Floor(shares[names[i]])
		fj := shares[names[j]] - math.Floor(shares[names[j]])
		if fi != fj {
			return fi > fj
		}
		return names[i] < names[j]
	})
	sum := 0.0
	for _, s := range shares {
		sum += s
	}
	budget := int(math.Min(float64(total), math.Floor(sum+1e-6)))
	for i := 0; used < budget && i < len(names); i++ {
		out[names[i]]++
		used++
	}
	return out
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
