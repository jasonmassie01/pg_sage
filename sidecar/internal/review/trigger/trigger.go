// Package trigger is Sage Review's trigger queue (review-04a): a review is
// worth most when someone has a decision to make, so events start one
// (an upgrade readiness request, an unresolved regression, a growth
// anomaly, a large migration, an instance change, enrollment) and the
// calendar is the fallback. Triggers are durable rows of
// sage.review_trigger; the leader claims them with FOR UPDATE SKIP LOCKED,
// coalesces those inside review.min_interval_hours into one review run,
// and records which run consumed each one ("why now"). Nothing here
// changes the monitored database: a review only reads and proposes.
package trigger

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"
)

// Kind is what fired a trigger.
type Kind string

// Trigger kinds; sage.review_trigger's check constraint holds the same list.
const (
	KindEnrollment           Kind = "enrollment"
	KindPreUpgrade           Kind = "pre_upgrade"
	KindUnresolvedRegression Kind = "unresolved_regression"
	KindCostGrowth           Kind = "cost_growth_anomaly"
	KindLargeMigration       Kind = "large_migration"
	KindInstanceChange       Kind = "instance_change"
	KindCalendar             Kind = "calendar"
)

var kinds = []Kind{KindEnrollment, KindPreUpgrade, KindUnresolvedRegression,
	KindCostGrowth, KindLargeMigration, KindInstanceChange, KindCalendar}

// Kinds is every trigger kind, in a fixed order.
func Kinds() []Kind { return append([]Kind(nil), kinds...) }

// Valid reports whether k is a known kind.
func (k Kind) Valid() bool {
	for _, v := range kinds {
		if k == v {
			return true
		}
	}
	return false
}

// Event reports whether k is an event rather than the calendar.
func (k Kind) Event() bool { return k.Valid() && k != KindCalendar }

// State is where a trigger is in the queue.
type State string

// Trigger states: fired -> claimed -> consumed or coalesced; an expired
// or released claim returns to fired.
const (
	StateFired     State = "fired"
	StateClaimed   State = "claimed"
	StateConsumed  State = "consumed"
	StateCoalesced State = "coalesced"
)

// Trigger is one row of sage.review_trigger. Key dedupes firing per
// database and kind; Detail is a JSON object (the "why now").
type Trigger struct {
	ID            int64          `json:"id"`
	Database      string         `json:"database"`
	Kind          Kind           `json:"kind"`
	Key           string         `json:"key"`
	Detail        map[string]any `json:"detail"`
	FiredAt       time.Time      `json:"fired_at"`
	State         State          `json:"state"`
	RunID         int64          `json:"run_id,omitempty"`
	CoalescedInto int64          `json:"coalesced_into,omitempty"`
}

// Errors, each distinguishable with errors.Is.
var (
	ErrInvalid         = errors.New("review trigger: invalid trigger")
	ErrNoStore         = errors.New("review trigger: no database connection pool")
	ErrLeaseLost       = errors.New("review trigger: the claim is no longer held")
	ErrUnknownDatabase = errors.New("review trigger: no review queue for the database")
	// ErrBusy is a Reviewer's answer when another run of the database is
	// running: the claim is released and the triggers wait.
	ErrBusy = errors.New("review trigger: a review run is already running")
)

// Run is one review run to start: its database and the triggers it
// consumes. OnStart, when set, must be called with the run's id once the
// run is recorded and before it does its work, so a restart finds the run
// a claim started.
type Run struct {
	Database string
	Triggers []Trigger
	OnStart  func(ctx context.Context, runID int64) error
}

// Kinds is the run's distinct trigger kinds, sorted.
func (r Run) Kinds() []string {
	seen := map[string]bool{}
	out := []string{}
	for _, t := range r.Triggers {
		if k := string(t.Kind); !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// Reviewer runs one review and returns its run id. An error wrapping
// ErrBusy means another run of the database is running.
type Reviewer interface {
	Review(ctx context.Context, run Run) (int64, error)
}

// counters are the process's trigger metrics.
var counters = struct {
	mu         sync.Mutex
	fireErrors map[Kind]int64
	runs       map[string]int64
}{fireErrors: map[Kind]int64{}, runs: map[string]int64{}}

func countFireError(k Kind) {
	counters.mu.Lock()
	defer counters.mu.Unlock()
	counters.fireErrors[k]++
}

// countRun records a started review run by cause: "event" when any of
// its triggers is an event, else "calendar".
func countRun(ts []Trigger) {
	cause := "calendar"
	for _, t := range ts {
		if t.Kind.Event() {
			cause = "event"
		}
	}
	counters.mu.Lock()
	defer counters.mu.Unlock()
	counters.runs[cause]++
}

// FireErrors is the count of failed trigger inserts by kind since start.
func FireErrors() map[Kind]int64 {
	counters.mu.Lock()
	defer counters.mu.Unlock()
	out := make(map[Kind]int64, len(counters.fireErrors))
	for k, v := range counters.fireErrors {
		out[k] = v
	}
	return out
}

// RunsStarted is the count of trigger-started review runs by cause
// ("event", "calendar") since start.
func RunsStarted() map[string]int64 {
	counters.mu.Lock()
	defer counters.mu.Unlock()
	out := make(map[string]int64, len(counters.runs))
	for k, v := range counters.runs {
		out[k] = v
	}
	return out
}
