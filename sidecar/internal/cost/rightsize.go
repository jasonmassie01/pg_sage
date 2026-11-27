package cost

import (
	"fmt"
	"sort"
	"time"

	"github.com/pg-sage/sidecar/internal/cloudtel"
)

// Right-sizing decisions.
const (
	DecisionDownsize = "downsize"
	DecisionUpsize   = "upsize"
	DecisionKeep     = "keep"
	DecisionUnknown  = "unknown"
)

// The rule's thresholds (market-04).
const (
	minDays          = 14.0
	minCoverage      = 0.9
	maxUsageAge      = 2 * time.Hour // newest hour older than this: stale
	downMaxP95Pct    = 60.0
	downMaxPeakPct   = 85.0
	upP95Pct         = 85.0
	upHotDays        = 3
	upLookbackDays   = 7
	reasonServerless = "unknown: capacity telemetry not collected"
)

// HourRow is one sage.host_usage_hourly row.
type HourRow struct {
	Hour                  time.Time
	InstanceClass         string
	VCPUs                 int
	Serverless            bool
	MemoryTotalBytes      float64
	AllocatedStorageBytes float64
	CPUP95, CPUMax        *float64
	MemAvailableMin       *float64
	DBLoadP95             *float64
	Samples               int
}

// RightsizeInput is one instance's recorded usage.
type RightsizeInput struct {
	Resource           string
	Rows               []HourRow
	SharedBuffersBytes float64
	Now                time.Time
	Rates              *Rates // nil: no dollar delta
}

// Verdict is the rule's decision and the evidence behind it.
type Verdict struct {
	Decision, Reason                 string
	CurrentClass, TargetClass        string
	CurrentVCPUs, TargetVCPUs        int
	Days, Coverage                   float64
	CPUP95, CPUMax                   float64
	DBLoadP95                        *float64
	ProjectedCPUP95, ProjectedCPUMax float64
	MonthlyDelta                     *float64
	Currency, RateSource             string
	AsOf                             time.Time
}

type usageStats struct {
	cpuP95, cpuMax *float64
	loadP95        *float64
	usedP95        *float64
	hotDays        int
}

// Rightsize applies the deterministic rule to the window of the current
// size: the newest rows of one class (a resize ends the old window).
func Rightsize(in RightsizeInput) Verdict {
	win := currentWindow(in.Rows)
	if len(win) == 0 {
		return Verdict{Decision: DecisionUnknown,
			Reason: "unknown: no hourly host usage recorded"}
	}
	last := win[len(win)-1]
	v := Verdict{CurrentClass: last.InstanceClass, CurrentVCPUs: last.VCPUs}
	span := last.Hour.Sub(win[0].Hour) + time.Hour
	v.Days, v.Coverage = span.Hours()/24, float64(len(win))/span.Hours()
	if reason := windowProblem(last, v, in.Now); reason != "" {
		v.Decision, v.Reason = DecisionUnknown, reason
		return v
	}
	st := summarize(win)
	if st.cpuP95 == nil || st.cpuMax == nil {
		v.Decision, v.Reason = DecisionUnknown, "unknown: no CPU history for this size"
		return v
	}
	v.CPUP95, v.CPUMax, v.DBLoadP95 = *st.cpuP95, *st.cpuMax, st.loadP95
	if reason := upsizeReason(st, last.VCPUs); reason != "" {
		return v.toward(nextLarger(last.InstanceClass), DecisionUpsize, reason, in.Rates)
	}
	return downsize(v, st, in)
}

// windowProblem says why the window cannot be judged ("" when it can).
func windowProblem(last HourRow, v Verdict, now time.Time) string {
	switch {
	case last.Serverless:
		return reasonServerless
	case last.VCPUs <= 0:
		return fmt.Sprintf("unknown: size %q has no documented vCPU count",
			last.InstanceClass)
	case now.Sub(last.Hour) > maxUsageAge:
		return fmt.Sprintf("unknown: host usage is stale (newest hour %s)",
			last.Hour.Format(time.RFC3339))
	case v.Days < minDays:
		return fmt.Sprintf("unknown: %.1f days of this size recorded; needs %.0f days",
			v.Days, minDays)
	case v.Coverage < minCoverage:
		return fmt.Sprintf("unknown: %.0f%% of hours recorded; needs %.0f%%",
			100*v.Coverage, 100*minCoverage)
	}
	return ""
}

// currentWindow sorts rows and keeps the newest run of one size.
func currentWindow(rows []HourRow) []HourRow {
	sorted := append([]HourRow(nil), rows...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Hour.Before(sorted[j].Hour) })
	if len(sorted) == 0 {
		return nil
	}
	last := sorted[len(sorted)-1]
	i := len(sorted) - 1
	for i > 0 && sorted[i-1].InstanceClass == last.InstanceClass &&
		sorted[i-1].VCPUs == last.VCPUs && sorted[i-1].Serverless == last.Serverless {
		i--
	}
	return sorted[i:]
}

func summarize(win []HourRow) usageStats {
	var p95s, maxes, loads, used []float64
	for _, r := range win {
		if r.CPUP95 != nil {
			p95s = append(p95s, *r.CPUP95)
		}
		if r.CPUMax != nil {
			maxes = append(maxes, *r.CPUMax)
		}
		if r.DBLoadP95 != nil {
			loads = append(loads, *r.DBLoadP95)
		}
		if r.MemAvailableMin != nil && r.MemoryTotalBytes > 0 {
			used = append(used, r.MemoryTotalBytes-*r.MemAvailableMin)
		}
	}
	return usageStats{cpuP95: cloudtel.NearestRank(p95s, 0.95),
		cpuMax: cloudtel.NearestRank(maxes, 1), loadP95: cloudtel.NearestRank(loads, 0.95),
		usedP95: cloudtel.NearestRank(used, 0.95), hotDays: hotDays(win)}
}

// hotDays counts, among the last 7 trailing 24-hour periods, those whose
// p95 of hourly CPU p95 reaches upP95Pct.
func hotDays(win []HourRow) int {
	end := win[len(win)-1].Hour
	periods := make([][]float64, upLookbackDays)
	for _, r := range win {
		k := int(end.Sub(r.Hour) / (24 * time.Hour))
		if k < upLookbackDays && r.CPUP95 != nil {
			periods[k] = append(periods[k], *r.CPUP95)
		}
	}
	hot := 0
	for _, p := range periods {
		if v := cloudtel.NearestRank(p, 0.95); v != nil && *v >= upP95Pct {
			hot++
		}
	}
	return hot
}

func upsizeReason(st usageStats, vcpus int) string {
	if st.hotDays >= upHotDays {
		return fmt.Sprintf("CPU p95 reached %.0f%% on %d of the last %d days", upP95Pct,
			st.hotDays, upLookbackDays)
	}
	if st.loadP95 != nil && *st.loadP95 > float64(vcpus) {
		return fmt.Sprintf("DB load p95 %.1f exceeds the %d vCPUs", *st.loadP95, vcpus)
	}
	return ""
}
