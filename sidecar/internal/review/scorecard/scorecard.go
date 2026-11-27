// Package scorecard is Sage Review's scorecard (review-05a): per review,
// and overall since a time, how many plan steps each origin (generator,
// model, person) proposed and what became of them: accepted, verified
// improved, no change, inconclusive, regressed, reverted, unverifiable or
// still pending, also split by generator name. It is the product's proof
// and the evidence the model gate (internal/review/modelgate) reads. The
// reads are SQL (Store); the arithmetic is here, in Go.
package scorecard

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Origins of a plan step (sage.plan_step_origin).
const (
	OriginGenerator = "generator"
	OriginModel     = "model"
	OriginPerson    = "person"
)

// Verdicts of a plan step (sage.plan_step.verdict); "" is pending.
const (
	VerdictImproved     = "improved"
	VerdictNoChange     = "no_change"
	VerdictRegressed    = "regressed"
	VerdictInconclusive = "inconclusive"
	VerdictUnverifiable = "unverifiable"
	VerdictExpired      = "expired"
)

// Review run statuses the scorecard reads.
const (
	StatusDone    = "done"
	StatusPartial = "partial"
)

// ErrInvalid is a row the scorecard cannot count.
var ErrInvalid = errors.New("scorecard: invalid row")

// Run is one finished review. Models are the normalized ids of its done
// model runs (none: the model pass did not finish). Fixture is a review of
// a clone, bench or game-day database, which the gate never counts.
type Run struct {
	ID         int64     `json:"id"`
	Database   string    `json:"database"`
	Status     string    `json:"status"`
	FinishedAt time.Time `json:"finished_at"`
	Models     []string  `json:"models"`
	Fixture    bool      `json:"fixture"`
}

// Step is one proposed plan step of a review with what became of it.
type Step struct {
	RunID      int64
	Origin     string
	OriginRef  string // the generator's name, the model id or the person
	ActionType string
	SQL        string
	Accepted   bool // a person approved it on its own, or it was started
	Verdict    string
	Reverted   bool // any rollback of the step
}

// Counts is what became of one origin's steps. Decided is the steps the
// verifier judged improved, no change or regressed, plus every reverted
// step: the denominator of the gate's rates.
type Counts struct {
	Proposed     int `json:"proposed"`
	Accepted     int `json:"accepted"`
	Improved     int `json:"verified_improved"`
	NoChange     int `json:"no_change"`
	Inconclusive int `json:"inconclusive"`
	Regressed    int `json:"regressed"`
	Reverted     int `json:"reverted"`
	Unverifiable int `json:"unverifiable"`
	Pending      int `json:"pending"`
	Decided      int `json:"decided"`
}

// add counts s; an unknown verdict is ErrInvalid.
func (c *Counts) add(s Step) error {
	c.Proposed++
	if s.Accepted {
		c.Accepted++
	}
	decided := s.Reverted
	switch s.Verdict {
	case VerdictImproved:
		c.Improved++
		decided = true
	case VerdictNoChange:
		c.NoChange++
		decided = true
	case VerdictRegressed:
		c.Regressed++
		decided = true
	case VerdictInconclusive, VerdictExpired:
		c.Inconclusive++
	case VerdictUnverifiable:
		c.Unverifiable++
	case "":
		c.Pending++
	default:
		return fmt.Errorf("%w: step of review %d has verdict %q", ErrInvalid, s.RunID,
			s.Verdict)
	}
	if s.Reverted {
		c.Reverted++
	}
	if decided {
		c.Decided++
	}
	return nil
}

// Review is one review's scorecard. ModelOnlyAccepted is its accepted
// model steps that no generator of the review also proposed.
type Review struct {
	Run               Run               `json:"run"`
	ByOrigin          map[string]Counts `json:"by_origin"`
	ModelOnlyAccepted int               `json:"model_only_accepted"`
}

// Scorecard is a database's reviews since Since, newest first, and their
// sums.
type Scorecard struct {
	Database          string            `json:"database"`
	Since             time.Time         `json:"since"`
	Reviews           int               `json:"reviews"`
	ByOrigin          map[string]Counts `json:"by_origin"`
	ByGenerator       map[string]Counts `json:"by_generator"`
	ModelOnlyAccepted int               `json:"model_only_accepted"`
	PerReview         []Review          `json:"per_review"`
}

// Build counts steps by the review runs they belong to. A step of a run
// not in runs, an unknown origin or verdict, or a step with no origin
// reference is ErrInvalid: a row the scorecard cannot place is never
// silently dropped or guessed.
func Build(database string, since time.Time, runs []Run, steps []Step) (Scorecard, error) {
	if strings.TrimSpace(database) == "" {
		return Scorecard{}, fmt.Errorf("%w: a scorecard names its database", ErrInvalid)
	}
	sc := Scorecard{Database: database, Since: since, ByOrigin: map[string]Counts{},
		ByGenerator: map[string]Counts{}, PerReview: []Review{}}
	at := make(map[int64]int, len(runs))
	sorted := append([]Run(nil), runs...)
	sort.SliceStable(sorted, func(i, j int) bool { return newer(sorted[i], sorted[j]) })
	for i, r := range sorted {
		at[r.ID] = i
		sc.PerReview = append(sc.PerReview, Review{Run: r, ByOrigin: map[string]Counts{}})
	}
	for _, s := range steps {
		i, ok := at[s.RunID]
		if !ok {
			return Scorecard{}, fmt.Errorf("%w: a step of review %d, which is not read",
				ErrInvalid, s.RunID)
		}
		if err := sc.count(&sc.PerReview[i], s); err != nil {
			return Scorecard{}, err
		}
	}
	for i := range sc.PerReview {
		n := modelOnlyAccepted(sc.PerReview[i].Run.ID, steps)
		sc.PerReview[i].ModelOnlyAccepted = n
		sc.ModelOnlyAccepted += n
	}
	sc.Reviews = len(sc.PerReview)
	return sc, nil
}

// newer orders reviews newest first, the higher id first on a tie.
func newer(a, b Run) bool {
	if !a.FinishedAt.Equal(b.FinishedAt) {
		return a.FinishedAt.After(b.FinishedAt)
	}
	return a.ID > b.ID
}

func (sc *Scorecard) count(r *Review, s Step) error {
	switch s.Origin {
	case OriginGenerator, OriginModel, OriginPerson:
	default:
		return fmt.Errorf("%w: a step of review %d has origin %q", ErrInvalid, s.RunID,
			s.Origin)
	}
	if strings.TrimSpace(s.OriginRef) == "" {
		return fmt.Errorf("%w: a %s step of review %d names no %s", ErrInvalid, s.Origin,
			s.RunID, s.Origin)
	}
	for _, m := range []map[string]Counts{r.ByOrigin, sc.ByOrigin} {
		c := m[s.Origin]
		if err := c.add(s); err != nil {
			return err
		}
		m[s.Origin] = c
	}
	if s.Origin == OriginGenerator {
		c := sc.ByGenerator[s.OriginRef]
		if err := c.add(s); err != nil {
			return err
		}
		sc.ByGenerator[s.OriginRef] = c
	}
	return nil
}

// modelOnlyAccepted is run's accepted model steps whose action and SQL no
// generator step of the same review also proposed.
func modelOnlyAccepted(run int64, steps []Step) int {
	generated := map[string]bool{}
	for _, s := range steps {
		if s.RunID == run && s.Origin == OriginGenerator {
			generated[s.ActionType+"\x00"+normalizeSQL(s.SQL)] = true
		}
	}
	n := 0
	for _, s := range steps {
		if s.RunID == run && s.Origin == OriginModel && s.Accepted &&
			!generated[s.ActionType+"\x00"+normalizeSQL(s.SQL)] {
			n++
		}
	}
	return n
}

// normalizeSQL compares statements by their words: lowercased, spacing
// collapsed and a trailing semicolon dropped.
func normalizeSQL(sql string) string {
	s := strings.ToLower(strings.Join(strings.Fields(sql), " "))
	return strings.TrimSpace(strings.TrimSuffix(s, ";"))
}
