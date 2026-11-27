package discovery

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/store"
)

// Enrollment by policy set (fleetpol-01b). With policy sets on, each
// candidate is matched to the highest-priority set its tags, creator and
// source match; each set enrolls its candidates on its own cycle
// (enrollment.interval_minutes), at most enrollment.per_cycle per cycle,
// with the owner its rule names and its TTL. Enrollment is unchanged
// otherwise: observation with approval execution, max_enrolled, the
// sage.databases cap and the claim lock between sidecars.

// SetLister reads the policy sets (policy.Store.Sets through
// PolicySetsFrom). An error enrolls nothing that cycle: fail closed.
type SetLister func(ctx context.Context) ([]policy.PolicySet, error)

// SetReader is what PolicySetsFrom reads (a *policy.Store).
type SetReader interface {
	Sets(ctx context.Context) ([]policy.SetDocument, error)
}

// PolicySetsFrom lists the sets of r.
func PolicySetsFrom(r SetReader) SetLister {
	return func(ctx context.Context) ([]policy.PolicySet, error) {
		docs, err := r.Sets(ctx)
		if err != nil {
			return nil, fmt.Errorf("reading policy sets for enrollment: %w", err)
		}
		out := make([]policy.PolicySet, 0, len(docs))
		for _, d := range docs {
			out = append(out, d.Set)
		}
		return out, nil
	}
}

// WithPolicySets turns on enrollment by policy set; grades, when set,
// asks for and records each member's first review-01 grade.
func (r *Runner) WithPolicySets(sets SetLister, grades Grades) *Runner {
	r.sets, r.grades = sets, grades
	return r
}

// Wait is how long Run waits after a cycle: def, or the shortest
// enrollment interval of the sets the last cycle read.
func (r *Runner) Wait(def time.Duration) time.Duration {
	r.statsMu.Lock()
	defer r.statsMu.Unlock()
	wait := def
	for _, s := range r.lastSets {
		wait = min(wait, setInterval(s))
	}
	return wait
}

// setInterval and setBatch bound what a stored set says, so a row edited
// past the API's validation cannot spin discovery or flood the fleet.
func setInterval(s policy.PolicySet) time.Duration {
	return max(s.Requirements.Enrollment.Interval(),
		policy.MinEnrollIntervalMinutes*time.Minute)
}

func setBatch(s policy.PolicySet) int {
	return min(max(s.Requirements.Enrollment.Batch(), 0), policy.MaxEnrollPerCycle)
}

// loadSets reads the sets for this cycle; ok is false when policy sets
// are off or could not be read.
func (r *Runner) loadSets(ctx context.Context, rep *Report) ([]policy.PolicySet, bool) {
	if r.sets == nil {
		return nil, false
	}
	sets, err := r.sets(ctx)
	if err != nil {
		rep.Errors = append(rep.Errors, fmt.Errorf("enrollment by policy set paused this "+
			"cycle: %w", err))
		return nil, false
	}
	r.statsMu.Lock()
	r.lastSets = sets
	r.statsMu.Unlock()
	return sets, true
}

// annotate cleans every candidate's attributes and, with sets, names the
// set it matches. Each reason is reported once per process.
func (r *Runner) annotate(b Binding, cands []Candidate, sets []policy.PolicySet,
	setsOK bool, rep *Report) []Candidate {
	name := b.Source.Name()
	out := make([]Candidate, len(cands))
	for i, c := range cands {
		for _, p := range cleanAttributes(&c) {
			r.ignore(rep, name, c.ExternalID, p)
		}
		if setsOK {
			set, why := matchCandidate(sets, b, c)
			c.SetName = set
			if why != "" {
				r.ignore(rep, name, c.ExternalID, why)
			}
		}
		out[i] = c
	}
	return out
}

func (r *Runner) ignore(rep *Report, source, ext, why string) {
	key := source + "\x00" + ext + "\x00" + why
	if r.warned[key] {
		return
	}
	r.warned[key] = true
	rep.Ignored = append(rep.Ignored, fmt.Sprintf("%s/%s: %s", source, ext, why))
}

// matchCandidate is the set c matches as an enrolled member would: its
// tags with discovered_from, its source and its creator.
func matchCandidate(sets []policy.PolicySet, b Binding, c Candidate) (string, string) {
	tags := make(map[string]string, len(c.Tags)+1)
	for k, v := range c.Tags {
		tags[k] = v
	}
	if b.EnrolFrom != "" {
		tags["discovered_from"] = b.EnrolFrom
	}
	m := policy.Member{Name: c.DBName, Tags: tags, DiscoverySource: b.Source.Name(),
		Creator: c.Creator}
	set, ok, err := policy.MatchSet(sets, m)
	switch {
	case err != nil:
		return "", "not enrolled: " + err.Error()
	case !ok:
		return "", "not enrolled: no policy set matches it"
	}
	return set.Name, ""
}

// enrolSets enrolls, set by set from the highest priority, the
// candidates of every set whose cycle is due, from the sources listed.
func (r *Runner) enrolSets(ctx context.Context, sets []policy.PolicySet,
	listed map[string]bool, now time.Time, rep *Report) {
	if r.cfg.MaxEnrolled <= 0 {
		return
	}
	ordered := append([]policy.PolicySet(nil), sets...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].Priority > ordered[j].Priority
	})
	for _, set := range ordered {
		if last, ok := r.setRun[set.Name]; ok && now.Sub(last) < setInterval(set) {
			continue
		}
		r.setRun[set.Name] = now
		budget := setBatch(set)
		for _, b := range r.bindings {
			if budget <= 0 {
				break
			}
			if b.EnrolFrom != "" && listed[b.Source.Name()] {
				budget -= r.enrolSet(ctx, b, set, budget, now, rep)
			}
		}
	}
}

// enrolSet claims up to budget candidates of set from b and enrolls them;
// it returns how many it claimed.
func (r *Runner) enrolSet(ctx context.Context, b Binding, set policy.PolicySet, budget int,
	now time.Time, rep *Report) int {
	name := b.Source.Name()
	tier, maxLive := set.EnrollTier(), r.cfg.MaxEnrolled
	if tier == policy.EnrollTierObserveOnly {
		// Observe-only members count against their own 2,000, which the
		// member store enforces; max_enrolled bounds the full members.
		maxLive += store.MaxObserveOnlyMembers
	}
	rows, err := r.store.ClaimSet(ctx, name, set.Name, tier, budget, maxLive, now)
	if err != nil {
		rep.Errors = append(rep.Errors, fmt.Errorf("claiming %s candidates of %s: %w",
			set.Name, name, err))
		return budget
	}
	expires := now.Add(r.setTTL(set))
	for _, row := range rows {
		row.Owner, row.Tier = enrolOwner(set, row, b.Owner), tier
		if !r.enrolRow(ctx, b, row, expires, rep) {
			continue
		}
		if err := r.store.MarkMilestone(ctx, row.ID, store.MilestoneEnrolled, now); err != nil {
			rep.Errors = append(rep.Errors, err)
		}
		rep.PerSet[set.Name]++
	}
	return len(rows)
}

func (r *Runner) setTTL(set policy.PolicySet) time.Duration {
	if set.Requirements.TTLHours > 0 {
		return time.Duration(set.Requirements.TTLHours) * time.Hour
	}
	return r.cfg.TTL()
}

// enrolOwner is the member's owner by the set's rule, falling back to the
// source's owner so a member always has one.
func enrolOwner(set policy.PolicySet, row store.DiscoveredRow, sourceOwner string) string {
	var owner string
	switch set.Requirements.Enrollment.OwnerRule() {
	case policy.EnrollOwnerTag:
		owner = row.Tags["owner"]
	case policy.EnrollOwnerCreator:
		owner = row.Creator
	}
	if owner == "" {
		return sourceOwner
	}
	return owner
}

// ErrMemberNotReady: the member's runtime has not started its review
// loop yet; its first grade is asked for again next cycle.
var ErrMemberNotReady = errors.New("discovered member is not running yet")
