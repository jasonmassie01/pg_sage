package runway

import (
	"fmt"
	"math"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
)

// Multixact member space (guard-02a). Every multixact stores its members
// (the locking transactions) in a separate 32-bit offset space, which
// fills independently of the multixact ID age the wraparound runway
// watches: on PostgreSQL 14-18 aggressive autovacuum starts once half of
// it is in use, whatever the multixact age, and new multixacts are refused
// when it is full. No view exposes the space in use before PostgreSQL 19,
// so it is estimated: the members of up to MXMemberSampleMax multixacts,
// spaced evenly between the oldest (datminmxid) and the next one as of the
// last checkpoint, times the multixacts in that range. The consumption rate
// comes from successive next_multi_offset values (modulo 2^32).

// CategoryMXMembers is the multixact member runway's finding category.
const CategoryMXMembers = "forecast_multixact_member_runway"

// The finding's identifier and runway kind.
const (
	MXMembersIdentifier = "multixact_members"
	RunwayMXMembers     = "mxid_members"
)

// The member space and its limits.
const (
	// MXMemberSpace is the size of the 32-bit member offset space.
	MXMemberSpace = float64(1 << 32)
	// MXMemberSafeThreshold is where aggressive autovacuum starts
	// (MULTIXACT_MEMBER_SAFE_THRESHOLD, half of the space).
	MXMemberSafeThreshold = MXMemberSpace / 2
	// mxMembersPerSegment is one pg_multixact/members SLRU segment at 8 kB
	// pages (1,636 members a page, 32 pages a segment).
	mxMembersPerSegment = 52352
	// MXMemberHardLimit is where new multixacts are refused: the space
	// less up to two segments (the stop limit is rounded to a segment).
	MXMemberHardLimit = MXMemberSpace - 2*mxMembersPerSegment
)

// Sampling, horizons and retention (product defaults, no configuration
// key).
const (
	MXMemberSampleMax       = 200
	MXMemberHorizon         = 14 * 24 * time.Hour
	MXMemberCritical        = 3 * 24 * time.Hour
	DefaultMXMemberInterval = 5 * time.Minute
	MXMemberRetention       = 90 * 24 * time.Hour
)

// Reading statuses.
const (
	MXMeasured = "measured"
	MXUnknown  = "unknown"
)

// MXControl is the multixact state as of the last checkpoint, with the
// cluster's oldest multixact (the oldest datminmxid).
type MXControl struct {
	NextMXID     uint32
	NextOffset   uint32
	OldestMXID   uint32
	CheckpointAt time.Time
}

// MXMemberEstimate is one reading of the member space: measured, or
// unknown with the reason and the grant that would make it measurable.
type MXMemberEstimate struct {
	MXControl
	Status string
	Reason string
	Grant  string
	// Live is the multixacts between the oldest and the next one.
	Live    uint32
	Sampled int
	// MeanMembers is the sampled members per multixact; Members the
	// estimated members in use and StdErr that estimate's standard error.
	MeanMembers float64
	Members     float64
	StdErr      float64
}

// liveMultixacts is the multixacts from oldest up to next, modulo 2^32
// (a range that wraps skips 0, InvalidMultiXactId); zero when next does
// not follow oldest (the checkpoint's next predates a freeze that
// advanced datminmxid past it).
func liveMultixacts(oldest, next uint32) uint32 {
	d := next - oldest
	if int32(d) <= 0 {
		return 0
	}
	if next < oldest {
		d--
	}
	return d
}

// sampleIDs spaces up to max multixact ids evenly over the live range
// starting at oldest; every id when the range is no larger than max. Past
// 2^32 - 1 the ids continue at 1.
func sampleIDs(oldest, live uint32, max int) []int64 {
	n := uint64(live)
	if max <= 0 {
		return nil
	}
	if n > uint64(max) {
		n = uint64(max)
	}
	ids := make([]int64, 0, n)
	for i := uint64(0); i < n; i++ {
		id := uint64(oldest) + i*uint64(live)/n
		if id > math.MaxUint32 {
			id -= math.MaxUint32 // 2^32 maps to 1
		}
		ids = append(ids, int64(id))
	}
	return ids
}

// estimateMembers is the mean members per sampled multixact, the
// estimated members in use (mean x live) and its standard error, with the
// finite population correction: a census (every multixact sampled) has
// none.
func estimateMembers(live uint32, counts []int64) (mean, total, stderr float64) {
	n := float64(len(counts))
	if n == 0 || live == 0 {
		return 0, 0, 0
	}
	var sum float64
	for _, c := range counts {
		sum += float64(c)
	}
	mean = sum / n
	pop := float64(live)
	total = mean * pop
	if n < 2 || n >= pop {
		return mean, total, 0
	}
	var ss float64
	for _, c := range counts {
		ss += (float64(c) - mean) * (float64(c) - mean)
	}
	fpc := (pop - n) / (pop - 1)
	return mean, total, pop * math.Sqrt(ss/(n-1)/n*fpc)
}

// offsetDelta is the members consumed from prev to cur, modulo 2^32. It
// is false when the delta is half the space or more: the offset went
// back (a restore, a failover to another cluster, pg_resetwal), which is
// a reset of the series, not a rate.
func offsetDelta(prev, cur uint32) (uint32, bool) {
	d := cur - prev
	return d, d < 1<<31
}

// memberRate is the members consumed per second between two checkpoints;
// false without a positive interval or across a reset.
func memberRate(prev, cur MXControl) (float64, bool) {
	dt := cur.CheckpointAt.Sub(prev.CheckpointAt).Seconds()
	if !(dt > 0) {
		return 0, false
	}
	d, ok := offsetDelta(prev.NextOffset, cur.NextOffset)
	if !ok {
		return 0, false
	}
	return float64(d) / dt, true
}

// mxLimits are the safe threshold and hard limit a runway is measured to.
type mxLimits struct{ safe, hard float64 }

var defaultMXLimits = mxLimits{safe: MXMemberSafeThreshold, hard: MXMemberHardLimit}

// MXMemberRunway is a measured estimate's runway: Upper is the estimate
// plus two standard errors (the runway is projected from it, so a noisy
// sample warns early), the rate (NaN when unknown) and the seconds to the
// safe threshold and the hard limit (0 at or past it, NaN without a rate,
// +Inf when nothing is consumed). Severity follows the safe threshold.
type MXMemberRunway struct {
	Upper         float64
	RatePerS      float64
	SecondsToSafe float64
	SecondsToHard float64
	Severity      string
}

func mxRunway(e MXMemberEstimate, rate float64, rateOK bool, l mxLimits) MXMemberRunway {
	upper := math.Min(e.Members+2*e.StdErr, MXMemberSpace)
	r := MXMemberRunway{Upper: upper, RatePerS: math.NaN()}
	if rateOK {
		r.RatePerS = rate
	}
	r.SecondsToSafe = secondsTo(l.safe, upper, r.RatePerS)
	r.SecondsToHard = secondsTo(l.hard, upper, r.RatePerS)
	switch {
	case math.IsNaN(r.SecondsToSafe) || !within(r.SecondsToSafe, MXMemberHorizon):
	case within(r.SecondsToSafe, MXMemberCritical):
		r.Severity = SeverityCritical
	default:
		r.Severity = SeverityWarning
	}
	return r
}

// secondsTo is the time from used to limit at rate (NaN: unknown).
func secondsTo(limit, used, rate float64) float64 {
	switch {
	case used >= limit:
		return 0
	case math.IsNaN(rate):
		return math.NaN()
	case rate <= 0:
		return math.Inf(1)
	}
	return (limit - used) / rate
}

// mxFinding is the forecast finding of a runway inside its horizon. It
// carries no SQL: nothing acts on a forecast directly.
func mxFinding(e MXMemberEstimate, r MXMemberRunway, l mxLimits,
	scope clusterScope) analyzer.Finding {
	detail := map[string]any{"runway_kind": RunwayMXMembers,
		"seconds_to_limit": round2(r.SecondsToSafe), "rate_per_s": finiteOrNil(r.RatePerS),
		"seconds_to_hard_limit": finiteOrNil(r.SecondsToHard),
		"limit":                 l.safe, "hard_limit": l.hard, "last_value": round2(r.Upper),
		"members_est": round2(e.Members), "members_stderr": round2(e.StdErr),
		"members_upper": round2(r.Upper), "live_multixacts": e.Live, "sampled": e.Sampled,
		"mean_members": round2(e.MeanMembers), "next_multixact_id": e.NextMXID,
		"next_multi_offset": e.NextOffset, "oldest_multixact_id": e.OldestMXID,
		"checkpoint_at": e.CheckpointAt.UTC().Format(time.RFC3339),
		"horizon_s":     MXMemberHorizon.Seconds()}
	if scope.shared {
		detail = scope.annotate(detail) // the owner reports the cluster's space
	}
	return analyzer.Finding{Category: CategoryMXMembers, Severity: r.Severity,
		ObjectType: "database", ObjectIdentifier: MXMembersIdentifier,
		Title: mxTitle(e, r, l), Detail: detail,
		Recommendation: mxRecommendation(r, l), ActionRisk: "safe"}
}

func mxTitle(e MXMemberEstimate, r MXMemberRunway, l mxLimits) string {
	head := fmt.Sprintf("Multixact member space: about %s members in use (+/- %s)",
		approxCount(e.Members), approxCount(e.StdErr))
	if r.SecondsToSafe <= 0 {
		return fmt.Sprintf("%s, past the aggressive-vacuum threshold (%s)", head,
			approxCount(l.safe))
	}
	when := (time.Duration(r.SecondsToSafe) * time.Second).Round(time.Minute)
	return fmt.Sprintf("%s; the aggressive-vacuum threshold (%s) is in about %s", head,
		approxCount(l.safe), when)
}

func mxRecommendation(r MXMemberRunway, l mxLimits) string {
	hard := "unknown"
	if !math.IsNaN(r.SecondsToHard) && !math.IsInf(r.SecondsToHard, 1) {
		hard = (time.Duration(r.SecondsToHard) * time.Second).Round(time.Minute).String()
	}
	return fmt.Sprintf("Multixact members (row locks shared by several transactions: "+
		"SELECT ... FOR SHARE or FOR KEY SHARE, foreign key checks) fill a space separate "+
		"from the multixact age. Past %s members autovacuum freezes multixacts "+
		"aggressively; at %s PostgreSQL refuses new multixacts (in about %s at the "+
		"measured rate). Find the sessions that hold shared row locks for long, and let "+
		"VACUUM (FREEZE) advance the oldest datminmxid. The estimate is sampled; it is "+
		"projected from its upper bound (estimate + 2 standard errors).",
		approxCount(l.safe), approxCount(l.hard), hard)
}
