package candidate

import (
	"fmt"
	"slices"

	"github.com/pg-sage/sidecar/internal/evidence/workpack"
)

// Coverage statuses.
const (
	CoverageComplete = "complete"
	CoveragePartial  = "partial"
)

// Coverage is how much of its input a generator saw. Missing names the
// pack sections it needed and did not find; Notes say what else was left
// out and why. WindowDays is the pack window's length in whole days.
type Coverage struct {
	Status     string   `json:"status"`
	WindowDays int      `json:"window_days"`
	Missing    []string `json:"missing,omitempty"`
	Notes      []string `json:"notes,omitempty"`
}

// AddMissing records a missing input once; the coverage is then partial.
func (c *Coverage) AddMissing(input string) {
	c.Status = CoveragePartial
	if !slices.Contains(c.Missing, input) {
		c.Missing = append(c.Missing, input)
	}
}

// Note records why something was left out without making it partial.
func (c *Coverage) Note(format string, args ...any) {
	c.Notes = append(c.Notes, fmt.Sprintf(format, args...))
}

// WindowCoverage is the coverage of a pack against review.window_days: a
// window shorter than windowDays (the history did not reach back that far)
// is partial. A nil pack is partial with "pack" missing.
func WindowCoverage(pack *workpack.Pack, windowDays int) Coverage {
	cov := Coverage{Status: CoverageComplete}
	if pack == nil {
		cov.AddMissing("pack")
		return cov
	}
	cov.WindowDays = WindowDays(pack)
	if cov.WindowDays < windowDays {
		cov.Status = CoveragePartial
		cov.Note("the pack covers %d days, review.window_days is %d", cov.WindowDays,
			windowDays)
	}
	return cov
}

// WindowDays is the pack window's length in whole days.
func WindowDays(pack *workpack.Pack) int {
	return int(pack.Window.To.Sub(pack.Window.From).Hours() / 24)
}
