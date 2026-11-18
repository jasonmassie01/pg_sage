package fleet

import (
	"context"
	"fmt"
	"slices"
	"sync"
)

// Rollup is tier 1: one member's deterministic summary. ID is its rollup
// id (R1.. by database name), the handle a triage reply cites. Kinds are
// the "generator/kind" pairs of its candidates, TopImpact the highest
// deterministic candidate score (report.Score). Fingerprint is its
// schema-shape key from fleet learning ("" when not fingerprinted).
// MonthlyUSD is set only with operator rates. Error is a read failure: the
// member is kept, and never judged an outlier.
type Rollup struct {
	ID          string         `json:"id"`
	Database    string         `json:"database"`
	Graded      bool           `json:"graded"`
	Grade       string         `json:"grade,omitempty"`
	Score       float64        `json:"score"`
	PrevScore   *float64       `json:"prev_score,omitempty"`
	CoveragePct float64        `json:"coverage_pct"`
	Candidates  map[string]int `json:"candidates,omitempty"`
	Kinds       []string       `json:"kinds,omitempty"`
	TopImpact   float64        `json:"top_impact"`
	IdleKnown   bool           `json:"idle_known"`
	Idle        bool           `json:"idle"`
	QuietDays   int            `json:"quiet_days"`
	SizeBytes   int64          `json:"size_bytes"`
	MonthlyUSD  *float64       `json:"monthly_usd,omitempty"`
	Fingerprint string         `json:"fingerprint,omitempty"`
	Error       string         `json:"error,omitempty"`
}

// GradeDrop is how many points the grade fell since the previous review
// (0 without one, or when it rose).
func (r Rollup) GradeDrop() float64 {
	if !r.Graded || r.PrevScore == nil || *r.PrevScore <= r.Score {
		return 0
	}
	return *r.PrevScore - r.Score
}

// Source reads one member's rollup.
type Source interface {
	Rollup(ctx context.Context, database string) (Rollup, error)
}

// BuildRollups reads every member's rollup with workers in parallel
// (at least 1). Members are deduplicated and sorted, blanks dropped; the
// result is in that order with ids R1..Rn, and is the same for any number
// of workers. A failed read is kept with its error.
func BuildRollups(ctx context.Context, members []string, src Source, workers int) []Rollup {
	names := uniqueMembers(members)
	out := make([]Rollup, len(names))
	if len(names) == 0 {
		return out
	}
	workers = min(max(workers, 1), len(names))
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				out[i] = readOne(ctx, src, names[i])
			}
		}()
	}
	for i := range names {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	for i := range out {
		out[i].ID, out[i].Database = fmt.Sprintf("R%d", i+1), names[i]
	}
	return out
}

func readOne(ctx context.Context, src Source, name string) Rollup {
	if err := ctx.Err(); err != nil {
		return Rollup{Error: fmt.Sprintf("rollup of %s: %v", name, err)}
	}
	if src == nil {
		return Rollup{Error: "rollup of " + name + ": no rollup source"}
	}
	r, err := src.Rollup(ctx, name)
	if err != nil {
		return Rollup{Error: fmt.Sprintf("rollup of %s: %v", name, err)}
	}
	return r
}

func uniqueMembers(members []string) []string {
	out := make([]string, 0, len(members))
	for _, m := range members {
		if m != "" {
			out = append(out, m)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}
