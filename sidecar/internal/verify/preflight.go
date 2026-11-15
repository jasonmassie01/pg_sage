package verify

import (
	"errors"
	"fmt"
	"slices"
	"time"
)

// Up-front unverifiable (lab-01c). Before a step runs, its targets'
// hourly history (evid-01's sage.query_rollup_hourly) says whether a watch
// can judge it within HardMax: for each hour of [From, From+HardMax), the
// median over the last PreflightWeeks weeks of the targets' calls at the
// same hour of the week. Expected calls below min_samples, or fewer than
// MinActiveHours hours with calls, make the step unverifiable: it then
// needs approval at every trust level. No history at all is unknown, which
// never blocks and is never read as either answer.

// Preflight statuses.
const (
	PreflightStatusVerifiable   = "verifiable"
	PreflightStatusUnverifiable = "unverifiable"
	PreflightStatusUnknown      = "unknown"
)

// PreflightWeeks is how many earlier weeks the expectation reads;
// MinActiveHours the fewest hours with calls a watch can judge.
const (
	PreflightWeeks = 4
	MinActiveHours = 3
)

// PreflightInput is the targets, their hourly rollup rows over the
// PreflightWeeks weeks before From, the step time and the watch's bounds.
type PreflightInput struct {
	Targets    []int64
	Hourly     Series
	From       time.Time
	HardMax    time.Duration
	MinSamples int
}

// Preflight is the up-front answer with the numbers behind it.
type Preflight struct {
	Status        string
	ExpectedCalls float64
	ActiveHours   int
	WeeklyCalls   float64
	WeeksSeen     int
	MinSamples    int
	HardMax       time.Duration
	Reason        string
}

// Blocks reports a step that may run only with an operator's approval.
func (p Preflight) Blocks() bool { return p.Status == PreflightStatusUnverifiable }

// PreflightExempt reports a class judged immediately after it runs
// (VACUUM, ANALYZE, REINDEX), which needs no watch window.
func PreflightExempt(class string) bool {
	return class == ClassVacuum || class == ClassAnalyze || class == ClassReindex
}

// PreflightVerifiable is the up-front answer for in. It refuses a zero
// step time and a non-positive HardMax or min_samples.
func PreflightVerifiable(in PreflightInput) (Preflight, error) {
	if in.From.IsZero() || in.HardMax <= 0 || in.MinSamples <= 0 {
		return Preflight{}, errors.New("verify: preflight needs the step time, a positive " +
			"hard max and a positive min_samples")
	}
	p := Preflight{MinSamples: in.MinSamples, HardMax: in.HardMax}
	if len(in.Targets) == 0 {
		p.Status, p.Reason = PreflightStatusUnknown, "no target statements to check"
		return p, nil
	}
	byWeek := weekHours(in)
	p.WeeksSeen = len(byWeek)
	if p.WeeksSeen == 0 {
		p.Status, p.Reason = PreflightStatusUnknown, "no hourly history for the targets "+
			"(a new database or new statements): not inferred either way"
		return p, nil
	}
	for _, calls := range byWeek {
		for _, c := range calls {
			p.WeeklyCalls += c
		}
	}
	p.WeeklyCalls /= float64(p.WeeksSeen)
	from := in.From.UTC().Truncate(time.Hour)
	for h := from; h.Before(in.From.Add(in.HardMax)); h = h.Add(time.Hour) {
		m := medianOverWeeks(byWeek, h)
		p.ExpectedCalls += m
		if m > 0 {
			p.ActiveHours++
		}
	}
	p.Status, p.Reason = preflightVerdict(p)
	return p, nil
}

// weekHours sums the targets' calls per hour, keyed by the week back
// (1..PreflightWeeks) the hour lies in; a week without any row is absent.
func weekHours(in PreflightInput) map[int]map[time.Time]float64 {
	from := in.From.UTC()
	out := map[int]map[time.Time]float64{}
	for _, id := range in.Targets {
		for _, b := range in.Hourly[id] {
			if b.Calls <= 0 {
				continue
			}
			at := b.Start.UTC().Truncate(time.Hour)
			back := from.Sub(at)
			if back <= 0 || back > PreflightWeeks*week {
				continue
			}
			k := int((back + week - 1) / week)
			if out[k] == nil {
				out[k] = map[time.Time]float64{}
			}
			out[k][at.Add(time.Duration(k)*week)] += float64(b.Calls)
		}
	}
	return out
}

// medianOverWeeks is the median, over the weeks seen, of the calls at the
// hour h shifted back by each week (0 in a seen week without that hour).
func medianOverWeeks(byWeek map[int]map[time.Time]float64, h time.Time) float64 {
	values := make([]float64, 0, len(byWeek))
	for _, hours := range byWeek {
		values = append(values, hours[h])
	}
	slices.Sort(values)
	n := len(values)
	if n%2 == 1 {
		return values[n/2]
	}
	return (values[n/2-1] + values[n/2]) / 2
}

func preflightVerdict(p Preflight) (string, string) {
	hours := int(p.HardMax.Round(time.Hour).Hours())
	reason := fmt.Sprintf("cannot be verified within %d h: the targets run %.0f times a "+
		"week (%.0f expected calls in %d active hour%s; %d calls and %d active hours "+
		"needed)", hours, p.WeeklyCalls, p.ExpectedCalls, p.ActiveHours,
		plural(p.ActiveHours), p.MinSamples, MinActiveHours)
	if p.ExpectedCalls < float64(p.MinSamples) || p.ActiveHours < MinActiveHours {
		return PreflightStatusUnverifiable, reason
	}
	return PreflightStatusVerifiable, fmt.Sprintf("%.0f expected calls in %d active hours "+
		"within %d h", p.ExpectedCalls, p.ActiveHours, hours)
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// Evidence is the preflight as JSON-safe data.
func (p Preflight) Evidence() map[string]any {
	return map[string]any{"status": p.Status, "expected_calls": p.ExpectedCalls,
		"active_hours": p.ActiveHours, "weekly_calls": p.WeeklyCalls,
		"weeks_seen": p.WeeksSeen, "min_samples": p.MinSamples,
		"hard_max_hours": p.HardMax.Hours(), "reason": p.Reason}
}
