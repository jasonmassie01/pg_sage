package fleet

import (
	"cmp"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/pg-sage/sidecar/internal/llm"
)

// triageLine is one member as the triage model reads it.
type triageLine struct {
	ID         string         `json:"id"`
	Database   string         `json:"db"`
	Grade      string         `json:"grade,omitempty"`
	Score      float64        `json:"score"`
	Drop       float64        `json:"drop"`
	Coverage   float64        `json:"coverage_pct"`
	Candidates map[string]int `json:"candidates,omitempty"`
	TopImpact  float64        `json:"top_impact"`
	Idle       *bool          `json:"idle,omitempty"`
	QuietDays  int            `json:"quiet_days"`
	SizeBytes  int64          `json:"size_bytes"`
	MonthlyUSD *float64       `json:"monthly_usd,omitempty"`
	Unread     bool           `json:"unread,omitempty"`
}

func lineOf(r Rollup) triageLine {
	l := triageLine{ID: r.ID, Database: r.Database, Grade: r.Grade, Score: r.Score,
		Drop: r.GradeDrop(), Coverage: r.CoveragePct, Candidates: r.Candidates,
		TopImpact: r.TopImpact, QuietDays: r.QuietDays, SizeBytes: r.SizeBytes,
		MonthlyUSD: r.MonthlyUSD, Unread: r.Error != ""}
	if r.IdleKnown {
		idle := r.Idle
		l.Idle = &idle
	}
	return l
}

// triagePrompt is a batch's members, one JSON object per line, wrapped as
// untrusted data.
func triagePrompt(batch []Rollup) string {
	lines := make([]string, 0, len(batch))
	for _, r := range batch {
		b, err := json.Marshal(lineOf(r))
		if err != nil {
			continue
		}
		lines = append(lines, string(b))
	}
	return fmt.Sprintf("Rollup of %d fleet members:\n%s", len(lines),
		llm.UntrustedData("fleet_rollup", strings.Join(lines, "\n")))
}

// DeterministicTriage is the no-model triage: the members that meet the
// outlier rule, ranked by grade drop, then candidate impact, then cost.
func DeterministicTriage(rollups []Rollup) []Outlier {
	return deterministicOf(rollups, medianCost(rollups))
}

func deterministicOf(rollups []Rollup, median float64) []Outlier {
	var out []Outlier
	for _, r := range rollups {
		why := outlierReasons(r, median)
		if len(why) == 0 {
			continue
		}
		out = append(out, Outlier{ID: r.ID, Database: r.Database, Score: needScore(r, median),
			Reason: strings.Join(why, "; "), Cites: []string{r.ID}, Source: ModeDeterministic})
	}
	byRule := make(map[string]Rollup, len(rollups))
	for _, r := range rollups {
		byRule[r.Database] = r
	}
	slices.SortStableFunc(out, func(a, b Outlier) int {
		ra, rb := byRule[a.Database], byRule[b.Database]
		return cmp.Or(cmp.Compare(rb.GradeDrop(), ra.GradeDrop()),
			cmp.Compare(rb.TopImpact, ra.TopImpact),
			cmp.Compare(costRatio(rb, median), costRatio(ra, median)),
			cmp.Compare(a.Database, b.Database))
	})
	return out
}

func outlierReasons(r Rollup, median float64) []string {
	if r.Error != "" || !r.Graded {
		return nil
	}
	var why []string
	if d := r.GradeDrop(); d >= DropOutlierPoints {
		why = append(why, fmt.Sprintf("grade fell %.1f points", d))
	}
	if r.Score < failingScore {
		why = append(why, fmt.Sprintf("grade %s (%.1f)", r.Grade, r.Score))
	}
	if r.TopImpact >= ImpactOutlier {
		why = append(why, fmt.Sprintf("a candidate scores %.1f", r.TopImpact))
	}
	if ratio := costRatio(r, median); ratio >= CostOutlierRatio {
		why = append(why, fmt.Sprintf("costs %.1fx the fleet median", ratio))
	}
	return why
}

func costRatio(r Rollup, median float64) float64 {
	if r.MonthlyUSD == nil || median <= 0 || math.IsNaN(*r.MonthlyUSD) {
		return 0
	}
	return *r.MonthlyUSD / median
}

// needScore is a member's need weight for the deep tokens.
func needScore(r Rollup, median float64) float64 {
	return 1 + r.GradeDrop() + max(r.TopImpact, 0) + costRatio(r, median)
}

// sortOutliers orders outliers by need, highest first, ties by name.
func sortOutliers(os []Outlier) {
	slices.SortStableFunc(os, func(a, b Outlier) int {
		return cmp.Or(cmp.Compare(b.Score, a.Score), cmp.Compare(a.Database, b.Database))
	})
}
