// Package discovery finds databases, branches and cloud instances a
// meta-db fleet could monitor (market-06). Sources list candidates; the
// Runner records them, enrols the ones whose credentials can be
// established at trust observation with approval, disables members whose
// TTL lapses unadopted or whose source reports them gone, and has no path
// that raises trust.
package discovery

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/store"
)

// Candidate is one database a source reported.
type Candidate = store.DiscoveredCandidate

// Source lists candidates. A listing is bounded: at most the cap the
// source was built with.
type Source interface {
	Name() string
	List(ctx context.Context) ([]Candidate, error)
}

// Binding is a source with its owner and, when its candidates may be
// enrolled, the member whose credentials they reuse.
type Binding struct {
	Source    Source
	Owner     string
	EnrolFrom string // "" records candidates only
}

// Store is the discovery state (store.DiscoveryStore).
type Store interface {
	Upsert(ctx context.Context, source, owner string, cands []Candidate,
		now time.Time) (int, error)
	MarkGone(ctx context.Context, source string, seenBefore time.Time) (
		[]store.DiscoveredRow, error)
	RecordSourceRun(ctx context.Context, source string, count int, runErr error,
		now time.Time) error
	Claim(ctx context.Context, source string, limit, maxLive int, now time.Time) (
		[]store.DiscoveredRow, error)
	MarkEnrolled(ctx context.Context, rowID int64, databaseID int) error
	Release(ctx context.Context, rowID int64, reason string) error
	DueForExpiry(ctx context.Context, now time.Time) ([]store.DiscoveredRow, error)
	SetStatus(ctx context.Context, rowID int64, status, reason string) error
	// Enrollment by policy set (fleetpol-01b).
	ClaimSet(ctx context.Context, source, set string, limit, maxLive int, now time.Time) (
		[]store.DiscoveredRow, error)
	PendingGrades(ctx context.Context) ([]store.DiscoveredRow, error)
	MarkMilestone(ctx context.Context, rowID int64, m store.Milestone, at time.Time) error
	EnrollStats(ctx context.Context, now time.Time) ([]store.EnrollStat, error)
}

// Members enrols and disables fleet members. Enrol must create the member
// at trust observation with approval execution.
type Members interface {
	Enrol(ctx context.Context, from string, row store.DiscoveredRow,
		expiresAt time.Time) (int, error)
	Disable(ctx context.Context, databaseID int) error
}

// Bounds of one cycle.
const (
	CycleCap      = 200 // candidates per source listing
	EnrolPerCycle = 5   // enrolments per source per cycle
	listTimeout   = 30 * time.Second
)

// Report is what one cycle did. With policy sets: PerSet is the members
// each set enrolled, Graded and AtBaseline the milestones recorded,
// Ignored the attributes and candidates left out and Overdue the members
// past their set's grade deadline (each reported once per process).
type Report struct {
	Sources    map[string]SourceReport
	Enrolled   int
	Expired    int
	Errors     []error
	PerSet     map[string]int
	Graded     int
	AtBaseline int
	Ignored    []string
	Overdue    []string
}

// SourceReport is one source's listing in a cycle. BackingOff: the source
// was not listed, its provider throttled an earlier listing.
type SourceReport struct {
	Listed, New, Gone int
	Truncated         bool
	BackingOff        bool
	Err               error
}

// Runner runs discovery cycles. Cycles are serialised.
type Runner struct {
	cfg      config.FleetDiscoveryConfig
	bindings []Binding
	store    Store
	members  Members
	now      func() time.Time
	cap      int
	mu       sync.Mutex
	listed   map[string]bool // sources with a successful listing this process
	backoff  map[string]backoffState
	// Enrollment by policy set (fleetpol-01b); sets nil is the legacy
	// per-source enrollment.
	sets     SetLister
	grades   Grades
	setRun   map[string]time.Time // each set's last enrollment cycle
	warned   map[string]bool      // reasons already reported
	statsMu  sync.Mutex           // guards lastSets and stats
	lastSets []policy.PolicySet
	stats    []store.EnrollStat
}

// NewRunner validates cfg and the bindings. A disabled runner needs no
// store or members and never lists.
func NewRunner(cfg config.FleetDiscoveryConfig, bindings []Binding, st Store,
	members Members) (*Runner, error) {
	r := &Runner{cfg: cfg, bindings: bindings, store: st, members: members,
		now: time.Now, cap: CycleCap, listed: map[string]bool{},
		backoff: map[string]backoffState{}, setRun: map[string]time.Time{},
		warned: map[string]bool{}}
	if !cfg.Enabled {
		return r, nil
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if st == nil || members == nil {
		return nil, errors.New("fleet discovery needs the meta-db store and fleet members")
	}
	for i, b := range bindings {
		if b.Source == nil {
			return nil, fmt.Errorf("discovery binding %d has no source", i)
		}
		if b.Owner == "" {
			return nil, fmt.Errorf("discovery source %q has no owner", b.Source.Name())
		}
	}
	return r, nil
}

// WithClock sets the clock (tests).
func (r *Runner) WithClock(now func() time.Time) *Runner {
	r.now = now
	return r
}

// WithCap sets the listing size at which a listing counts as truncated.
func (r *Runner) WithCap(n int) *Runner {
	r.cap = n
	return r
}

// Run runs a cycle now and then, until ctx ends, after each Wait: the
// interval, or the shortest enrollment interval of the policy sets.
func (r *Runner) Run(ctx context.Context, interval time.Duration) {
	if !r.cfg.Enabled {
		return
	}
	for {
		logReport(r.Cycle(ctx))
		t := time.NewTimer(r.Wait(interval))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

func logReport(rep Report) {
	for name, s := range rep.Sources {
		if s.Err != nil {
			slog.Warn("fleet discovery: listing failed; previous candidates kept",
				"source", name, "error", s.Err)
		}
	}
	for _, err := range rep.Errors {
		slog.Warn("fleet discovery", "error", err)
	}
	for _, why := range rep.Ignored {
		slog.Warn("fleet discovery: ignored", "reason", why)
	}
	for _, late := range rep.Overdue {
		slog.Warn("fleet discovery: first grade overdue", "member", late)
	}
}

// Cycle lists every source, records what it found, marks what vanished
// gone, enrols within max_enrolled (by policy set when they are on),
// advances first grades, and disables expired members.
func (r *Runner) Cycle(ctx context.Context) Report {
	rep := Report{Sources: map[string]SourceReport{}, PerSet: map[string]int{}}
	if !r.cfg.Enabled {
		return rep
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	sets, setsOK := r.loadSets(ctx, &rep)
	listed := map[string]bool{}
	for _, b := range r.bindings {
		name := b.Source.Name()
		sr := r.listSource(ctx, b, now, sets, setsOK, &rep)
		rep.Sources[name] = sr
		listed[name] = sr.Err == nil && !sr.BackingOff
		if r.sets == nil && listed[name] && b.EnrolFrom != "" && r.cfg.MaxEnrolled > 0 {
			r.enrol(ctx, b, now, &rep)
		}
	}
	if setsOK {
		r.enrolSets(ctx, sets, listed, now, &rep)
		r.grade(ctx, sets, now, &rep)
	}
	r.expire(ctx, now, &rep)
	if r.sets != nil {
		r.refreshStats(ctx, now, &rep)
	}
	return rep
}

// listSource lists one source. A failed listing keeps every candidate; a
// first or truncated listing marks nothing gone; a throttled one backs
// the source off.
func (r *Runner) listSource(ctx context.Context, b Binding, now time.Time,
	sets []policy.PolicySet, setsOK bool, rep *Report) SourceReport {
	name := b.Source.Name()
	if r.backingOff(name, now) {
		return SourceReport{BackingOff: true}
	}
	lctx, cancel := context.WithTimeout(ctx, listTimeout)
	cands, err := b.Source.List(lctx)
	cancel()
	r.noteListing(name, err, now)
	var sr SourceReport
	if err == nil {
		cands = r.annotate(b, cands, sets, setsOK, rep)
		sr.Listed, sr.Truncated = len(cands), len(cands) >= r.cap
		sr.New, err = r.store.Upsert(ctx, name, b.Owner, cands, now)
	}
	if err != nil {
		sr.Err = fmt.Errorf("discovery source %s: %w", name, err)
	} else if r.listed[name] && !sr.Truncated {
		sr.Gone = r.markGone(ctx, name, now, rep)
	}
	if err == nil {
		r.listed[name] = true
	}
	if recErr := r.store.RecordSourceRun(ctx, name, sr.Listed, sr.Err, now); recErr != nil {
		rep.Errors = append(rep.Errors, recErr)
	}
	return sr
}

func (r *Runner) markGone(ctx context.Context, name string, now time.Time,
	rep *Report) int {
	gone, err := r.store.MarkGone(ctx, name, now)
	if err != nil {
		rep.Errors = append(rep.Errors, err)
	}
	for _, g := range gone {
		if g.DatabaseID == nil {
			continue
		}
		if err := r.members.Disable(ctx, *g.DatabaseID); err != nil {
			rep.Errors = append(rep.Errors, fmt.Errorf(
				"disabling gone member %d of %s: %w", *g.DatabaseID, name, err))
		}
	}
	return len(gone)
}

func (r *Runner) enrol(ctx context.Context, b Binding, now time.Time, rep *Report) {
	name := b.Source.Name()
	rows, err := r.store.Claim(ctx, name, EnrolPerCycle, r.cfg.MaxEnrolled, now)
	if err != nil {
		rep.Errors = append(rep.Errors, fmt.Errorf("claiming candidates of %s: %w",
			name, err))
		return
	}
	expires := now.Add(r.cfg.TTL())
	for _, row := range rows {
		r.enrolRow(ctx, b, row, expires, rep)
	}
}

// enrolRow enrolls one claimed row until expires; a failed enrolment
// releases the claim with the error.
func (r *Runner) enrolRow(ctx context.Context, b Binding, row store.DiscoveredRow,
	expires time.Time, rep *Report) bool {
	id, err := r.members.Enrol(ctx, b.EnrolFrom, row, expires)
	if err == nil {
		err = r.recordEnrolled(ctx, row, id)
	} else if relErr := r.store.Release(ctx, row.ID, err.Error()); relErr != nil {
		err = errors.Join(err, relErr)
	}
	if err != nil {
		rep.Errors = append(rep.Errors, fmt.Errorf("enrolling %s/%s: %w", b.Source.Name(),
			row.ExternalID, err))
		return false
	}
	rep.Enrolled++
	return true
}

// recordEnrolled links row to its new member. When the link cannot be
// written (the meta-db failed, or another sidecar marked the row gone) the
// member is disabled again: an unlinked member would never expire.
func (r *Runner) recordEnrolled(ctx context.Context, row store.DiscoveredRow,
	databaseID int) error {
	err := r.store.MarkEnrolled(ctx, row.ID, databaseID)
	if err == nil {
		return nil
	}
	if disErr := r.members.Disable(ctx, databaseID); disErr != nil {
		return errors.Join(err, fmt.Errorf("disabling unlinked member %d: %w",
			databaseID, disErr))
	}
	return fmt.Errorf("%w (member %d disabled again)", err, databaseID)
}

// expire disables every enrolled member whose TTL lapsed unadopted.
func (r *Runner) expire(ctx context.Context, now time.Time, rep *Report) {
	due, err := r.store.DueForExpiry(ctx, now)
	if err != nil {
		rep.Errors = append(rep.Errors, err)
		return
	}
	for _, row := range due {
		err := r.members.Disable(ctx, *row.DatabaseID)
		if err == nil {
			err = r.store.SetStatus(ctx, row.ID, store.DiscoveryExpired,
				"ttl elapsed without an owner adopting it")
		}
		if err != nil {
			rep.Errors = append(rep.Errors, fmt.Errorf("expiring member %d: %w",
				*row.DatabaseID, err))
			continue
		}
		rep.Expired++
	}
}
