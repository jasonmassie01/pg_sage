package fleet

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/cost"
	"github.com/pg-sage/sidecar/internal/store"
)

// The sprawl census (fleetpol-03). Once a month the fleet leader lists
// every database of a meta-db fleet (full members, observe-only members
// and discovered databases never enrolled) with its owner, creator,
// creation time, last real activity, size and monthly cost at the
// operator's rates, and answers one number: the share of agent-created
// databases with any real activity seven or more days after creation.
// Real activity is review-02f's idle-database rule: workload calls,
// pg_sage's own statements excluded. A discovered member no owner has
// adopted and with no real activity for 30 days gets a TTL proposal,
// which is advice for a person: pg_sage deletes nothing and has no
// delete action. The census only reads monitored databases.

// Census subject kinds.
const (
	CensusFull        = store.TierFull
	CensusObserveOnly = store.TierObserveOnly
	CensusDiscovered  = "discovered" // found by discovery, never enrolled
)

// Census row statuses.
const (
	CensusMeasured    = "measured"
	CensusUnmeasured  = "unmeasured"
	CensusUnreachable = "unreachable"
	CensusNotEnrolled = "not_enrolled"
)

// Census rules.
const (
	CensusQuietWindow = 30 * 24 * time.Hour  // quiet this long: propose a TTL
	CensusProposedTTL = 7 * 24 * time.Hour   // the TTL proposed
	CensusReuseAfter  = 7 * 24 * time.Hour   // activity this long after creation is reuse
	CensusRetention   = 400 * 24 * time.Hour // runs older than this are pruned
	CensusUnpriced    = "unpriced"
	censusAgentPrefix = "agent:"
	maxCensusNoteLen  = 1000
)

// ErrCensusUnreachable marks a measurement that could not reach the
// database; the census lists it unreachable and goes on.
var ErrCensusUnreachable = errors.New("census: database unreachable")

// CensusSubject is one database the census lists.
type CensusSubject struct {
	Key        string // member name, or "<source>/<external id>" never enrolled
	Kind       string
	Creator    string // "agent:..." or "person:..." from discovery; "" unknown
	Owner      string
	CreatedAt  *time.Time // the provider's creation time, else registration
	Discovered bool       // found by a discovery source
	Adopted    bool       // an owner adopted the member
}

// CensusMeasure is what the census read of a subject's activity and size.
type CensusMeasure struct {
	LastActivityAt *time.Time    // newest real activity seen; nil: none seen
	ObservedFrom   *time.Time    // start of the unbroken observation
	QuietFor       time.Duration // observed without real activity, up to now
	SizeBytes      *int64        // as the source reported it (negative is invalid)
	Basis          string        // what activity was read from
	Unmeasured     string        // non-empty: activity is unknown, and why
}

// CensusRow is one database in a census run.
type CensusRow struct {
	DatabaseKey    string
	Kind           string
	Status         string
	Creator        string
	Owner          string
	CreatedAt      *time.Time
	LastActivityAt *time.Time
	ActivityBasis  string
	Reused         *bool // nil: not agent-created, too young, or unknown
	SizeBytes      *int64
	SizeNote       string
	CostMonth      *float64
	CostNote       string
	Proposal       string
	Note           string
}

// CensusRun is a run's summary.
type CensusRun struct {
	ID            int64
	Period        time.Time // the UTC month the run is for
	Holder        string
	RunAt         time.Time
	Databases     int
	AgentCreated  int
	ReuseEligible int
	Reused        int
	ReuseShare    *float64 // Reused / ReuseEligible; nil when none is eligible
	Proposals     int
	Unreachable   int
}

// CensusPeriod is the UTC calendar month of t.
func CensusPeriod(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// CensusRowFor is subject's row from its measurement (or the error that
// stopped it), priced at the operator's rates.
func CensusRowFor(s CensusSubject, m CensusMeasure, err error, rates cost.RateSource,
	now time.Time) CensusRow {
	row := CensusRow{DatabaseKey: s.Key, Kind: s.Kind, Creator: s.Creator, Owner: s.Owner,
		CreatedAt: s.CreatedAt}
	switch {
	case s.Kind == CensusDiscovered:
		row.Status = CensusNotEnrolled
		row.Note = "discovered, never enrolled: pg_sage has no connection to it"
		m = CensusMeasure{}
	case err != nil && (errors.Is(err, ErrCensusUnreachable) ||
		errors.Is(err, ErrObserveUnreachable)):
		row.Status, row.Note, m = CensusUnreachable, clipNote(err.Error()), CensusMeasure{}
	case err != nil:
		row.Status, row.Note, m = CensusUnmeasured, clipNote(err.Error()), CensusMeasure{}
	case m.Unmeasured != "":
		row.Status, row.Note = CensusUnmeasured, clipNote(m.Unmeasured)
	default:
		row.Status, row.Note = CensusMeasured, activityNote(m)
		row.LastActivityAt, row.ActivityBasis = m.LastActivityAt, m.Basis
		row.Reused = reused(s, m, now)
		row.Proposal = proposal(s, m)
	}
	row.SizeBytes, row.SizeNote = censusSize(m)
	row.CostMonth, row.CostNote = censusCost(rates, s.Key, row.SizeBytes)
	return row
}

func activityNote(m CensusMeasure) string {
	switch {
	case m.LastActivityAt != nil:
		return "last real activity " + m.LastActivityAt.UTC().Format(time.RFC3339)
	case m.ObservedFrom != nil:
		return "no real activity observed since " + m.ObservedFrom.UTC().Format(time.RFC3339)
	}
	return "no real activity observed"
}

// reused is the reuse rule: an agent-created database at least
// CensusReuseAfter old reused when real activity was seen at or after
// creation plus CensusReuseAfter; not reused when none was seen and the
// observation covers that moment; otherwise unknown.
func reused(s CensusSubject, m CensusMeasure, now time.Time) *bool {
	if !strings.HasPrefix(s.Creator, censusAgentPrefix) || s.CreatedAt == nil {
		return nil
	}
	mark := s.CreatedAt.Add(CensusReuseAfter)
	if mark.After(now) {
		return nil
	}
	yes, no := true, false
	switch {
	case m.LastActivityAt != nil && !m.LastActivityAt.Before(mark):
		return &yes
	case m.ObservedFrom != nil && !m.ObservedFrom.After(mark):
		return &no
	}
	return nil
}

// proposal is the TTL advice: a discovered member no owner adopted, quiet
// for CensusQuietWindow or more. A person's database is listed, never
// proposed.
func proposal(s CensusSubject, m CensusMeasure) string {
	if !s.Discovered || s.Adopted || s.Kind == CensusDiscovered ||
		m.QuietFor < CensusQuietWindow {
		return ""
	}
	return fmt.Sprintf("no real activity in %d days: propose TTL %d days",
		int(m.QuietFor/(24*time.Hour)), int(CensusProposedTTL/(24*time.Hour)))
}

func censusSize(m CensusMeasure) (*int64, string) {
	switch {
	case m.SizeBytes == nil:
		return nil, "not measured"
	case *m.SizeBytes < 0:
		from := m.Basis
		if from == "" {
			from = "the size source"
		}
		return nil, fmt.Sprintf("invalid size %d from %s: stored as unknown", *m.SizeBytes,
			from)
	}
	v := *m.SizeBytes
	return &v, ""
}

// censusCost is the monthly storage cost at the operator's storage rate
// (pg_sage ships no price tables), or why it is unpriced.
func censusCost(rates cost.RateSource, key string, size *int64) (*float64, string) {
	if rates == nil {
		return nil, CensusUnpriced + ": no operator rates (cost.rates)"
	}
	r, ok := rates.Rates("", key)
	switch {
	case !ok:
		return nil, CensusUnpriced + ": no cost.rates entry for " + key
	case r.StorageGBMonth <= 0:
		return nil, CensusUnpriced + ": the cost.rates entry for " + key +
			" has no storage rate"
	case size == nil:
		return nil, CensusUnpriced + ": size not measured"
	}
	v := float64(*size) / float64(1<<30) * r.StorageGBMonth
	return &v, fmt.Sprintf("%s per month at the operator's storage rate (%s, as of %s)",
		r.Currency, r.Source, r.AsOf.Format(time.DateOnly))
}

// SummarizeCensus counts rows into run's summary.
func SummarizeCensus(run CensusRun, rows []CensusRow) CensusRun {
	run.Databases = len(rows)
	run.AgentCreated, run.ReuseEligible, run.Reused = 0, 0, 0
	run.Proposals, run.Unreachable, run.ReuseShare = 0, 0, nil
	for _, r := range rows {
		if strings.HasPrefix(r.Creator, censusAgentPrefix) {
			run.AgentCreated++
		}
		if r.Reused != nil {
			run.ReuseEligible++
			if *r.Reused {
				run.Reused++
			}
		}
		if r.Proposal != "" {
			run.Proposals++
		}
		if r.Status == CensusUnreachable {
			run.Unreachable++
		}
	}
	if run.ReuseEligible > 0 {
		share := float64(run.Reused) / float64(run.ReuseEligible)
		run.ReuseShare = &share
	}
	return run
}

func clipNote(s string) string {
	if len(s) > maxCensusNoteLen {
		return s[:maxCensusNoteLen]
	}
	return s
}
