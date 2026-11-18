package connections

import (
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/pg-sage/sidecar/internal/review/candidate"
)

// The recycle herd rule: in at least HerdMinSamples samples, more than
// HerdShare (strictly) of an application's sessions connected in one
// minute. An application under HerdMinSessions sessions in a sample is not
// judged there: two of three sessions sharing a minute is no storm.
const (
	HerdShare       = 0.30
	HerdMinSamples  = 3
	HerdMinSessions = 10
)

// herdSeen is one sample in which an application herded.
type herdSeen struct {
	at              time.Time
	inBucket, total int
}

// ageCounts are the skipped ages of the samples read.
type ageCounts struct{ unknown, negative, noAges int }

// herds finds each application's herd samples and emits one candidate per
// application with at least HerdMinSamples of them.
func (b *builder) herds() {
	seen := map[string][]herdSeen{}
	var skipped ageCounts
	for _, s := range b.in.Samples {
		if s.Ages == nil {
			skipped.noAges++
			continue
		}
		for app, h := range sampleHerds(s, &skipped) {
			seen[app] = append(seen[app], h)
		}
	}
	b.noteSkipped(skipped)
	apps := make([]string, 0, len(seen))
	for app, hs := range seen {
		if len(hs) >= HerdMinSamples {
			apps = append(apps, app)
		}
	}
	slices.Sort(apps)
	for _, app := range apps {
		b.herd(app, seen[app])
	}
}

// sampleHerds is the applications herding in s: their largest minute and
// the sessions it is judged against. A nil minute (backend_start not
// visible) and a negative one are skipped and counted. A truncated read is
// judged against the application's group total, so a cut list never
// inflates a share.
func sampleHerds(s Sample, skipped *ageCounts) map[string]herdSeen {
	buckets := map[string]map[int64]int{}
	valid := map[string]int{}
	for _, a := range s.Ages {
		switch {
		case a.Minute == nil:
			skipped.unknown += a.Sessions
		case *a.Minute < 0:
			skipped.negative += a.Sessions
		default:
			if buckets[a.Application] == nil {
				buckets[a.Application] = map[int64]int{}
			}
			buckets[a.Application][*a.Minute] += a.Sessions
			valid[a.Application] += a.Sessions
		}
	}
	out := map[string]herdSeen{}
	for app, by := range buckets {
		total := valid[app]
		if s.AgesTruncated {
			total = max(total, s.Apps[app])
		}
		top := 0
		for _, n := range by {
			top = max(top, n)
		}
		// More than 30%, in integers: 10 * top > 3 * total.
		if total >= HerdMinSessions && 10*top > 3*total {
			out[app] = herdSeen{at: s.At, inBucket: top, total: total}
		}
	}
	return out
}

func (b *builder) noteSkipped(s ageCounts) {
	if s.unknown > 0 {
		b.cov.Note("%d session ages were unknown (backend_start not visible to pg_sage's "+
			"role) and skipped", s.unknown)
	}
	if s.negative > 0 {
		b.cov.Note("%d session ages were negative (clock skew, or a session that connected "+
			"during the read) and skipped", s.negative)
	}
	if s.noAges > 0 {
		b.cov.Note("%d samples hold no connection ages by application (written before "+
			"they were collected); no herd was judged on them", s.noAges)
	}
}

// herd emits the recycle herd of app from the samples it herded in.
func (b *builder) herd(app string, hs []herdSeen) {
	best, times := hs[0], make([]time.Time, 0, len(hs))
	for _, h := range hs {
		times = append(times, h.at)
		if h.inBucket*best.total > best.inBucket*h.total {
			best = h
		}
	}
	share := math.Round(float64(best.inBucket)*1000/float64(best.total)) / 10
	reason := fmt.Sprintf("%d of %d sessions of %s connected in the same minute (%.1f%%, "+
		"at %s), and more than %.0f%% shared a minute in %d of %d samples: a recycle herd. "+
		"Sessions that connect together reach the pool's maximum lifetime together and "+
		"reconnect at once, a connection storm on the server. Jitter the pool's connection "+
		"lifetime (for example plus or minus 20%% around PgBouncer's server_lifetime or the "+
		"client pool's maxLifetime) so reconnects spread out. pg_sage never changes a pool.",
		best.inBucket, best.total, appName(app), share, best.at.UTC().Format(time.RFC3339),
		HerdShare*100, len(hs), len(b.in.Samples))
	b.emit(candidate.Candidate{Kind: KindRecycleHerd, Target: appTarget(app),
		ActionType: candidate.ActionAdvice, EvidenceIDs: b.v.hours(times), Reason: reason,
		Prediction: candidate.Prediction{Metric: "sessions_connecting_together",
			Direction: "decrease", Source: "workload_sessions connection ages by application",
			Quantities: map[string]float64{QuantityHerd: 1,
				QuantityHerdSamples: float64(len(hs)), QuantityHerdSessions: float64(best.inBucket),
				QuantityHerdSharePct: share}}})
}
