package drill

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/pg-sage/sidecar/internal/clone"
)

// Scheduler defaults.
const (
	// DefaultCadenceDays is a cheap drill's cadence without a policy value.
	DefaultCadenceDays = 30
	// DefaultMaxConcurrent is drill.max_concurrent_per_fleet's default.
	DefaultMaxConcurrent = 2
	// FailedRetryAfter is how soon a failed cheap drill is retried, so its
	// finding can resolve once the backup is fixed. A failed full copy
	// waits its cadence: each retry is a full instance bill.
	FailedRetryAfter = 24 * time.Hour
	// gateMarginDays is how far inside require_restore_drill_days the
	// shortened cadence keeps a drill (the plan's product call).
	gateMarginDays = 2
)

// DefaultKinds are the clone kinds a drill uses without a policy list:
// the cheap ones. A full copy needs clone.allow_full_copy and full_copy in
// the policy document's drill.kinds.
var DefaultKinds = []clone.Kind{clone.KindThinClone, clone.KindBranch, clone.KindCopyOnWrite}

// Target is one database the scheduler may drill.
type Target struct {
	Database string
	Enabled  bool
	// Kind is what the configured provider's clones are.
	Kind clone.Kind
	// AllowFullCopy is clone.allow_full_copy.
	AllowFullCopy bool
	// Kinds is the policy document's drill.kinds; nil is DefaultKinds.
	Kinds []clone.Kind
	// CadenceDays is drill.cadence_days; 0 is DefaultCadenceDays.
	CadenceDays int
	// GateProduction: the agent gate is on and the database is production,
	// so D7 needs a drill newer than RequireDrillDays.
	GateProduction   bool
	RequireDrillDays int
	// Run runs (and records) the drill.
	Run func(context.Context)
}

func cheap(k clone.Kind) bool {
	return k == clone.KindThinClone || k == clone.KindBranch || k == clone.KindCopyOnWrite
}

// Cadence is how often t is drilled: the policy cadence, shortened to
// require_restore_drill_days - 2 (at least a day) for a cheap kind when
// the agent gate needs a fresh drill, so a monthly default does not keep
// D7 at L2.
func (t Target) Cadence() time.Duration {
	days := t.CadenceDays
	if days <= 0 {
		days = DefaultCadenceDays
	}
	if t.GateProduction && cheap(t.Kind) && t.RequireDrillDays > 0 {
		days = min(days, max(1, t.RequireDrillDays-gateMarginDays))
	}
	return time.Duration(days) * 24 * time.Hour
}

// KindAllowed is nil when t's clone kind may be used for a drill.
func (t Target) KindAllowed() error {
	kinds := t.Kinds
	if kinds == nil {
		kinds = DefaultKinds
	}
	switch {
	case t.Kind == clone.KindSchemaOnly || t.Kind == "":
		return fmt.Errorf("clone kind %q holds no copy of the data", t.Kind)
	case !slices.Contains(kinds, t.Kind):
		return fmt.Errorf("clone kind %s is not in the policy document's drill.kinds",
			t.Kind)
	case t.Kind == clone.KindFullCopy && !t.AllowFullCopy:
		return errors.New("a full-copy drill also needs clone.allow_full_copy: true")
	case t.Kind != clone.KindFullCopy && !cheap(t.Kind):
		return fmt.Errorf("unknown clone kind %q", t.Kind)
	}
	return nil
}

// Due reports whether t should be drilled at now, given its newest
// attempt (found false: never drilled).
func (t Target) Due(last Attempt, found bool, now time.Time) bool {
	if !t.Enabled || t.KindAllowed() != nil {
		return false
	}
	if !found {
		return true
	}
	age := now.Sub(last.RequestedAt)
	if !last.Passed && cheap(t.Kind) && age >= FailedRetryAfter {
		return true
	}
	return age >= t.Cadence()
}

// History is the drills already run (Store).
type History interface {
	LastAttempt(ctx context.Context, database string) (Attempt, bool, error)
}

// Scheduler starts due drills: one per database at a time and at most
// MaxConcurrent per fleet. The caller runs it on the fleet leader only;
// the clone provider's shared create limit and backoff apply on top.
type Scheduler struct {
	History       History
	MaxConcurrent int
	Now           func() time.Time
	Logf          func(format string, args ...any)

	mu      sync.Mutex
	running map[string]bool
	wg      sync.WaitGroup
}

func (s *Scheduler) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Scheduler) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

// Pass starts every due target it may and returns the databases started.
func (s *Scheduler) Pass(ctx context.Context, targets []Target) []string {
	if s == nil {
		return nil
	}
	var due []Target
	for _, t := range targets {
		if t.Run == nil || t.Database == "" {
			continue
		}
		last, found, err := s.History.LastAttempt(ctx, t.Database)
		if err != nil {
			s.logf("restore drill scheduler: %s skipped this pass: %v", t.Database, err)
			continue
		}
		if t.Due(last, found, s.now()) {
			due = append(due, t)
		}
	}
	return s.start(ctx, due)
}

func (s *Scheduler) start(ctx context.Context, due []Target) []string {
	limit := s.MaxConcurrent
	if limit <= 0 {
		limit = DefaultMaxConcurrent
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running == nil {
		s.running = map[string]bool{}
	}
	var started []string
	for _, t := range due {
		if len(s.running) >= limit {
			break
		}
		if s.running[t.Database] {
			continue
		}
		s.running[t.Database] = true
		started = append(started, t.Database)
		s.wg.Add(1)
		go s.run(ctx, t)
	}
	return started
}

func (s *Scheduler) run(ctx context.Context, t Target) {
	defer s.wg.Done()
	defer func() {
		s.mu.Lock()
		delete(s.running, t.Database)
		s.mu.Unlock()
	}()
	t.Run(ctx)
}

// Running is the databases with a drill in progress, sorted.
func (s *Scheduler) Running() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.running))
	for db := range s.running {
		out = append(out, db)
	}
	sort.Strings(out)
	return out
}

// Wait blocks until every started drill finished.
func (s *Scheduler) Wait() { s.wg.Wait() }
