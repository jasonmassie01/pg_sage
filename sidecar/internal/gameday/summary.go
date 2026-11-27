package gameday

import (
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
)

// benches-08: a run whose fault programs all skipped (a snapshot clone
// without superuser, max_prepared_transactions = 0) is not a safety
// signal. Each run keeps what its report scored, and a run that scored
// nothing finishes completed_unscored instead of completed.

// Summary is what a run's report scored. A run is skipped when its fault
// program could not run on the target (the reason says why), errored when
// it failed, and scored otherwise.
type Summary struct {
	Scored  int `json:"scored"`
	Skipped int `json:"skipped"`
	Errored int `json:"errored"`
	// TopSkips are the most frequent skip reasons, at most maxTopSkips,
	// by count and then reason.
	TopSkips []SkipCount `json:"top_skips"`
}

// SkipCount is how many runs one reason skipped.
type SkipCount struct {
	Reason string `json:"reason"`
	Count  int    `json:"count"`
}

const (
	maxTopSkips = 5
	// maxSkipReason caps a reason, so one fault program cannot bloat a row.
	maxSkipReason = 200
)

// summarize counts a bench report's runs. A report it cannot read, or
// one without runs, scores nothing: it never reads as scored.
func summarize(raw []byte) Summary {
	var report struct {
		Runs []struct {
			Skipped string `json:"skipped"`
			Error   string `json:"error"`
		} `json:"runs"`
	}
	s := Summary{TopSkips: []SkipCount{}}
	if err := json.Unmarshal(raw, &report); err != nil {
		return s
	}
	reasons := map[string]int{}
	for _, run := range report.Runs {
		switch {
		case run.Skipped != "":
			s.Skipped++
			reasons[truncate(run.Skipped, maxSkipReason)]++
		case run.Error != "":
			s.Errored++
		default:
			s.Scored++
		}
	}
	for reason, n := range reasons {
		s.TopSkips = append(s.TopSkips, SkipCount{Reason: reason, Count: n})
	}
	sort.Slice(s.TopSkips, func(i, j int) bool {
		a, b := s.TopSkips[i], s.TopSkips[j]
		if a.Count != b.Count {
			return a.Count > b.Count
		}
		return a.Reason < b.Reason
	})
	if len(s.TopSkips) > maxTopSkips {
		s.TopSkips = s.TopSkips[:maxTopSkips]
	}
	return s
}

// scoredStatus is a finished run's status: a completed run that scored no
// run, or has no summary, is completed_unscored; any other status stands.
func scoredStatus(status string, s *Summary) string {
	if status == StatusCompleted && (s == nil || s.Scored == 0) {
		return StatusCompletedUnscored
	}
	return status
}

// warnUnscored logs one WARN for a run that scored nothing, naming its top
// skip reasons, so the operator can fix the target.
func warnUnscored(warn func(string, ...any), kind, id, database string, s *Summary) {
	if s == nil {
		s = &Summary{}
	}
	reasons := make([]string, 0, len(s.TopSkips))
	for _, r := range s.TopSkips {
		reasons = append(reasons, fmt.Sprintf("%s (%d)", r.Reason, r.Count))
	}
	top := strings.Join(reasons, "; ")
	if top == "" {
		top = "none"
	}
	warn("%s %s on %s: completed_unscored: 0 of %d runs scored (%d skipped, %d errored); "+
		"top skip reasons: %s", kind, id, database, s.Scored+s.Skipped+s.Errored, s.Skipped,
		s.Errored, top)
}

// defaultWarn logs a WARN line through the standard logger.
func defaultWarn(format string, args ...any) {
	log.Printf("[WARN] [gameday] "+format, args...)
}
