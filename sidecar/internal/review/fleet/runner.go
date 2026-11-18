package fleet

import (
	"context"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/llm"
)

// Ledger keeps the passes and the members' states (Store on the control
// database). Every state move refuses a member not in the state it needs
// with ErrState.
type Ledger interface {
	StartPass(ctx context.Context, period string, members int) (int64, error)
	SaveRollups(ctx context.Context, passID int64, rs []Rollup) error
	MarkTriaged(ctx context.Context, passID int64, os []Outlier) error
	ScheduledIn(ctx context.Context, period string) ([]string, error)
	Carried(ctx context.Context, period string) ([]Outlier, error)
	MarkScheduled(ctx context.Context, d Deep, ref int64) error
	MarkDeferred(ctx context.Context, passID int64, database, to string) error
	AwaitingReview(ctx context.Context) ([]Scheduled, error)
	MarkReviewed(ctx context.Context, passID int64, database string, runID int64) error
	FinishPass(ctx context.Context, passID int64, s PassSummary) error
	LastPass(ctx context.Context, period string) (string, bool, error)
}

// Scheduler starts a member's deep review and reports when a run did it.
type Scheduler interface {
	ScheduleDeep(ctx context.Context, d Deep) (int64, error)
	DeepStatus(ctx context.Context, database string, ref int64) (bool, int64, error)
}

// Settings bound a pass: DeepMax deep reviews a month, Workers parallel
// rollup reads, Price for the tiers' cost (nil: unpriced).
type Settings struct {
	DeepMax int
	Workers int
	Price   llm.PriceFunc
}

// PassSummary is what a finished pass records.
type PassSummary struct {
	Status     string      `json:"status"`
	Members    int         `json:"members"`
	Outliers   int         `json:"outliers"`
	Scheduled  int         `json:"scheduled"`
	Deferred   int         `json:"deferred"`
	DeepTokens int         `json:"deep_tokens"`
	Triage     Triage      `json:"triage"`
	Fleet      FleetReview `json:"fleet"`
	Error      string      `json:"error,omitempty"`
}

// Result is one Run or Advance.
type Result struct {
	PassID    int64             `json:"pass_id,omitempty"`
	Period    string            `json:"period"`
	Skipped   string            `json:"skipped,omitempty"`
	Members   int               `json:"members"`
	Outliers  int               `json:"outliers"`
	Scheduled []Deep            `json:"scheduled"`
	Deferred  []string          `json:"deferred"`
	Halted    bool              `json:"halted"`
	Reviewed  int               `json:"reviewed"`
	Failures  map[string]string `json:"failures,omitempty"`
	Triage    Triage            `json:"triage"`
	Fleet     FleetReview       `json:"fleet"`
}

// Runner runs the tiered pass. Stopped is the fleet's emergency stop
// (nil: never stopped): while it holds, nothing is scheduled.
type Runner struct {
	Ledger   Ledger
	Source   Source
	Deep     Scheduler
	Triage   Model
	Review   Model
	Stopped  func() bool
	Settings Settings
	Now      func() time.Time
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Runner) stopped() bool { return r.Stopped != nil && r.Stopped() }

func (r *Runner) usable() error {
	if r == nil || r.Ledger == nil || r.Source == nil || r.Deep == nil {
		return ErrNoLedger
	}
	return nil
}

// Run is one tiered pass over members. An empty fleet starts no pass. A
// fleet of one skips the model tiers (deterministic triage, no fleet
// review).
func (r *Runner) Run(ctx context.Context, members []string) (Result, error) {
	members = uniqueMembers(members)
	period := Period(r.now())
	res := Result{Period: period, Members: len(members)}
	if len(members) == 0 {
		res.Skipped = "empty fleet: no members to review"
		return res, nil
	}
	if err := r.usable(); err != nil {
		return res, err
	}
	// The pass starts first: starting closes a pass left running, whose
	// rows must not hide the members waiting from an earlier one.
	var err error
	if res.PassID, err = r.Ledger.StartPass(ctx, period, len(members)); err != nil {
		return res, fmt.Errorf("review fleet: start the %s pass: %w", period, err)
	}
	carried, err := r.Ledger.Carried(ctx, period)
	if err != nil {
		err = fmt.Errorf("read the members waiting for %s: %w", period, err)
		return res, r.finish(ctx, &res, err)
	}
	err = r.tiers(ctx, &res, members, carried)
	return res, r.finish(ctx, &res, err)
}

// tiers runs tiers 1 to 4 of a started pass.
func (r *Runner) tiers(ctx context.Context, res *Result, members []string,
	carried []Outlier) error {
	rollups := BuildRollups(ctx, members, r.Source, r.Settings.Workers)
	if err := r.Ledger.SaveRollups(ctx, res.PassID, rollups); err != nil {
		return fmt.Errorf("save the rollups of pass %d: %w", res.PassID, err)
	}
	triageModel, reviewModel := r.Triage, r.Review
	if len(rollups) < 2 {
		triageModel, reviewModel = nil, nil
	}
	res.Triage = RunTriage(ctx, triageModel, rollups, r.Settings.Price)
	if len(rollups) < 2 {
		res.Triage.Reasons = []string{"fleet of 1: deterministic triage, no model tiers"}
	}
	outliers := withCarried(res.PassID, carried, res.Triage.Outliers, members)
	res.Outliers = len(outliers)
	if err := r.Ledger.MarkTriaged(ctx, res.PassID, outliers); err != nil {
		return fmt.Errorf("record the triage of pass %d: %w", res.PassID, err)
	}
	res.Fleet = ReviewFleet(ctx, reviewModel, rollups, r.Settings.Price)
	return r.schedule(ctx, res, outliers)
}

// withCarried puts the waiting members first, then the triage's new
// outliers, each once, all in pass; a waiting member that left the fleet
// is dropped.
func withCarried(pass int64, carried, fresh []Outlier, members []string) []Outlier {
	in := make(map[string]bool, len(members))
	for _, m := range members {
		in[m] = true
	}
	seen := map[string]bool{}
	var out []Outlier
	for _, group := range [][]Outlier{carried, fresh} {
		for _, o := range group {
			if !in[o.Database] || seen[o.Database] {
				continue
			}
			seen[o.Database] = true
			o.PassID = pass
			out = append(out, o)
		}
	}
	return out
}

func (r *Runner) finish(ctx context.Context, res *Result, err error) error {
	s := PassSummary{Status: PassDone, Members: res.Members, Outliers: res.Outliers,
		Scheduled: len(res.Scheduled), Deferred: len(res.Deferred), Triage: res.Triage,
		Fleet: res.Fleet}
	for _, d := range res.Scheduled {
		s.DeepTokens += d.Tokens
	}
	if res.Halted {
		s.Status = PassHalted
	}
	if err != nil {
		s.Status, s.Error = PassFailed, err.Error()
	}
	ferr := r.Ledger.FinishPass(context.WithoutCancel(ctx), res.PassID, s)
	switch {
	case err != nil:
		return fmt.Errorf("review fleet: %w", err)
	case ferr != nil:
		return fmt.Errorf("review fleet: finish pass %d: %w", res.PassID, ferr)
	}
	return nil
}
