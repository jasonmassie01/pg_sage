package fleet

import (
	"cmp"
	"slices"
	"time"

	sagefleet "github.com/pg-sage/sidecar/internal/fleet"
)

// The need split of the deep tokens: every pick gets at least half and at
// most three times the even share.
const (
	deepFloorPct   = 50
	deepCeilingPct = 300
)

// Deep is one deep review scheduled for an outlier: its pass, the month
// it counts against and its token allowance.
type Deep struct {
	Database string  `json:"database"`
	PassID   int64   `json:"pass_id"`
	Tokens   int     `json:"tokens"`
	Score    float64 `json:"score"`
	Reason   string  `json:"reason"`
	Period   string  `json:"period"`
}

// Scheduled is a deep review waiting for its run; Ref is the scheduler's
// handle (the member's trigger id).
type Scheduled struct {
	Deep
	Ref int64 `json:"ref"`
}

// Period is t's calendar month in UTC, "2006-01".
func Period(t time.Time) string { return t.UTC().Format("2006-01") }

// NextPeriod is the month after p, "" when p is not a period.
func NextPeriod(p string) string {
	t, err := time.Parse("2006-01", p)
	if err != nil {
		return ""
	}
	return Period(t.AddDate(0, 1, 0))
}

// SelectDeep picks at most room outliers: carried ones first, then by
// need, ties by name. The rest wait.
func SelectDeep(outliers []Outlier, room int) (pick, rest []Outlier) {
	ordered := slices.Clone(outliers)
	slices.SortStableFunc(ordered, func(a, b Outlier) int {
		if a.Carried != b.Carried {
			if a.Carried {
				return -1
			}
			return 1
		}
		return cmp.Or(cmp.Compare(b.Score, a.Score), cmp.Compare(a.Database, b.Database))
	})
	room = min(max(room, 0), len(ordered))
	return ordered[:room], ordered[room:]
}

// AllocateTokens splits DeepTokensPerReview per pick across the picks by
// need (fleet.AllocateByNeed), keyed by database.
func AllocateTokens(pick []Outlier) map[string]int {
	weights := make(map[string]float64, len(pick))
	for _, o := range pick {
		weights[o.Database] = o.Score
	}
	return sagefleet.AllocateByNeed(len(pick)*DeepTokensPerReview, weights, deepFloorPct,
		deepCeilingPct)
}
