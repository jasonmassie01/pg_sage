package verify

import (
	"math"
	"time"

	"github.com/pg-sage/sidecar/internal/baseline"
)

// Reference levels of an attribution (lab-01a). Each statement is read
// against its own level (its pre-window level, or the seasonal expectation
// of the hour), and a bucket's pooled value is the weighted per-call values
// of the statements that ran in it over the same weights on their levels.
// A statement missing from a bucket leaves both sides, so which statements
// ran in a bucket cannot move the pooled value: the frozen weights hold
// even when a low-traffic target runs in only some buckets.

// levels are statements' reference levels in log space, by queryid and
// then by level key (the zero time for the pre-window's single level, the
// judged hour for a seasonal one).
type levels map[int64]map[time.Time]float64

func (l levels) set(id int64, key time.Time, v float64) {
	if l[id] == nil {
		l[id] = map[time.Time]float64{}
	}
	l[id][key] = v
}

// levelFn is a statement's reference value (not in log space) for the
// bucket that starts at t.
type levelFn func(id int64, t time.Time) (float64, bool)

// own returns, for a bucket start, the statement's own level.
func (l levels) own(key func(time.Time) time.Time) levelFn {
	return func(id int64, t time.Time) (float64, bool) {
		v, ok := l[id][key(t)]
		return math.Exp(v), ok
	}
}

// pooledOf returns the frozen weights' pooled level for every statement:
// the test-only post-weights switch compares the post-period mix against
// the pre-period mix, as v1 Pool does.
func (l levels) pooledOf(weights map[int64]float64, key func(time.Time) time.Time) levelFn {
	return func(_ int64, t time.Time) (float64, bool) {
		var num, den float64
		for _, id := range sortedKeys(weights) {
			if v, ok := l[id][key(t)]; ok {
				num += weights[id] * math.Exp(v)
				den += weights[id]
			}
		}
		return num / den, den > 0
	}
}

// pooled is, per bucket start, the log of the weighted per-call values of
// the statements that ran in the bucket and have a level, over the same
// weights on those levels; a bucket whose value is not positive is left
// out.
func pooled(s Series, weights map[int64]float64, m metric, level levelFn) map[time.Time]float64 {
	num, den := map[time.Time]float64{}, map[time.Time]float64{}
	for _, id := range sortedKeys(weights) {
		for _, b := range s[id] {
			r, ok := level(id, b.Start)
			if !ok || !(r > 0) || math.IsInf(r, 0) {
				continue
			}
			num[b.Start] += weights[id] * m(b)
			den[b.Start] += weights[id] * r
		}
	}
	out := make(map[time.Time]float64, len(num))
	for t, n := range num {
		if v := n / den[t]; v > 0 && !math.IsInf(v, 0) {
			out[t] = math.Log(v)
		}
	}
	return out
}

// ownLogs is a statement's log per-call value per bucket start; a bucket
// whose value is not positive is left out.
func ownLogs(bs []Bucket, m metric) map[time.Time]float64 {
	out := make(map[time.Time]float64, len(bs))
	for _, b := range bs {
		if v := m(b); v > 0 && !math.IsInf(v, 0) {
			out[b.Start] = math.Log(v)
		}
	}
	return out
}

// preLevels are the statements' mean log value over the pre-window.
func (u unit) preLevels(m metric) levels {
	out := levels{}
	for _, id := range sortedKeys(u.weights) {
		if own := ownLogs(u.pre[id], m); len(own) > 0 {
			out.set(id, time.Time{}, mean(values(own)))
		}
	}
	return out
}

// postHours are the hours of the targets' post buckets, in order.
func (u unit) postHours() []time.Time {
	hours := map[time.Time]float64{}
	for id := range u.weights {
		for _, b := range u.post[id] {
			hours[b.Start.Truncate(time.Hour)] = 0
		}
	}
	return sortedTimes(hours)
}

// seasonalLevels are, per statement and judged hour, the mean log value of
// the same hour in the earlier baseline.DefaultWeeks weeks; hourOf maps
// each earlier hour to the judged hour it stands for (the first when
// several could).
func (u unit) seasonalLevels(m metric) (levels, map[time.Time]time.Time) {
	hours, hourOf := u.postHours(), map[time.Time]time.Time{}
	for _, h := range hours {
		for k := 1; k <= baseline.DefaultWeeks; k++ {
			if at := h.Add(-time.Duration(k) * week); hourOf[at].IsZero() {
				hourOf[at] = h
			}
		}
	}
	out := levels{}
	for _, id := range sortedKeys(u.weights) {
		own := ownLogs(u.hist[id], m)
		for _, h := range hours {
			var vs []float64
			for k := 1; k <= baseline.DefaultWeeks; k++ {
				at := h.Add(-time.Duration(k) * week)
				if v, ok := own[at]; ok && hourOf[at].Equal(h) {
					vs = append(vs, v)
				}
			}
			if len(vs) > 0 {
				out.set(id, h, mean(vs))
			}
		}
	}
	return out, hourOf
}

// seasonalWeeks counts the earlier weeks (up to baseline.DefaultWeeks)
// with history of any target at any of the post period's hours.
func (u unit) seasonalWeeks() int {
	hours, n := u.postHours(), 0
	for k := 1; k <= baseline.DefaultWeeks; k++ {
		if u.historyAt(hours, time.Duration(k)*week) {
			n++
		}
	}
	return n
}

func (u unit) historyAt(hours []time.Time, back time.Duration) bool {
	for _, id := range sortedKeys(u.weights) {
		own := ownLogs(u.hist[id], blocksPerCall)
		for _, h := range hours {
			if _, ok := own[h.Add(-back)]; ok {
				return true
			}
		}
	}
	return false
}
