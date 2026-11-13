package cost

import (
	"math"
	"sort"
	"strconv"
	"time"
)

// QueryDelta is one query's execution time over a window, from
// consecutive sage.query_store samples of one statistics epoch.
type QueryDelta struct {
	Database string
	QueryID  int64
	ExecMs   float64
}

// QueryCost is one query's share of the instance's compute cost. The
// queryid is text: a 64-bit id does not survive a JSON number.
type QueryCost struct {
	Database    string  `json:"database"`
	QueryID     string  `json:"queryid"`
	ExecSeconds float64 `json:"exec_seconds"`
	Share       float64 `json:"share"`
	Cost        float64 `json:"cost"`
	Currency    string  `json:"currency,omitempty"`
}

// Attribution is a window's compute cost split over its queries.
type Attribution struct {
	ComputeCost      float64
	TotalExecSeconds float64
	// IdleShare is the part of the vCPU time no recorded query used (nil:
	// vCPUs unknown).
	IdleShare *float64
	Queries   []QueryCost // by cost, highest first
	Dropped   int         // negative or non-finite deltas, never charged
}

// Attribute charges computePerHour x window to the deltas in proportion
// to their execution time: a query's cost = compute cost x (Δexec / ΣΔ).
// idle_share = 1 - ΣΔexec / (window x vCPUs), at least 0.
func Attribute(computePerHour float64, window time.Duration, vcpus int,
	deltas []QueryDelta) Attribution {
	a := Attribution{Queries: []QueryCost{}}
	if window <= 0 {
		return a
	}
	a.ComputeCost = computePerHour * window.Hours()
	var total float64
	valid := make([]QueryDelta, 0, len(deltas))
	for _, d := range deltas {
		if math.IsNaN(d.ExecMs) || math.IsInf(d.ExecMs, 0) || d.ExecMs < 0 {
			a.Dropped++
			continue
		}
		if d.ExecMs > 0 {
			valid = append(valid, d)
			total += d.ExecMs
		}
	}
	a.TotalExecSeconds = total / 1000
	if vcpus > 0 {
		idle := max(0, 1-a.TotalExecSeconds/(window.Seconds()*float64(vcpus)))
		a.IdleShare = &idle
	}
	for _, d := range valid {
		share := d.ExecMs / total
		a.Queries = append(a.Queries, QueryCost{Database: d.Database,
			QueryID: strconv.FormatInt(d.QueryID, 10), ExecSeconds: d.ExecMs / 1000,
			Share: share, Cost: a.ComputeCost * share})
	}
	sort.SliceStable(a.Queries, func(i, j int) bool {
		return a.Queries[i].Cost > a.Queries[j].Cost
	})
	return a
}
