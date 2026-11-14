package lakebaseobs

import (
	"errors"
	"sort"
	"time"
)

// WaitLoad (market-01b) turns retained wait_event_counters events into the
// workload explorer's wait load: each event's wait_event_time already
// covers [previous_ts, occurred_at), so time waited per wall-clock second
// is the interval's average active sessions in that wait.

// WaitEventTimeUnit is the unit of wait_event_time. The repository does not
// document it; milliseconds follows the Postgres *_time column convention
// and stays unconfirmed until a live workspace check (market-01b CHECK-40).
const (
	WaitEventTimeUnit     = time.Millisecond
	WaitEventTimeUnitName = "ms"
)

// maxWaitLoadEvents bounds the events table of a wait-load answer.
const maxWaitLoadEvents = 20

// ErrHistoryUnavailable reports that a database has no Lakebase telemetry
// runtime to read history from (not a storage failure).
var ErrHistoryUnavailable = errors.New("history source unavailable")

// WaitLoadPoint is one counter interval: average active sessions by wait
// class over [From, T), summed across the computes that share its edges.
type WaitLoadPoint struct {
	From     time.Time          `json:"from"`
	T        time.Time          `json:"t"`
	Sessions float64            `json:"sessions"`
	ByClass  map[string]float64 `json:"by_class"`
}

// WaitLoadEvent is one wait event's total over the window.
type WaitLoadEvent struct {
	WaitClass   string  `json:"wait_event_type"`
	WaitEvent   string  `json:"wait_event"`
	WaitSeconds float64 `json:"wait_seconds"`
	Count       float64 `json:"wait_count"`
}

// WaitLoadSeries is the window's wait load, oldest interval first.
// Current is the load by wait class now: each compute's newest interval
// that overlaps the newest interval of all, summed (computes need not share
// interval edges), ending at CurrentAt. Skipped counts unusable intervals;
// Truncated reports that the read was capped and older intervals are missing.
type WaitLoadSeries struct {
	Points    []WaitLoadPoint    `json:"series"`
	Current   map[string]float64 `json:"current"`
	CurrentAt time.Time          `json:"current_at"`
	Classes   []string           `json:"classes"`
	Events    []WaitLoadEvent    `json:"events"`
	Skipped   int                `json:"skipped"`
	Truncated bool               `json:"truncated"`
}

type waitInterval struct {
	from, to     time.Time
	compute      string
	class, event string
	seconds      float64
	count        float64
}

// computeNewest is one compute's newest interval and its load by class.
type computeNewest struct {
	from, to time.Time
	byClass  map[string]float64
}

// add keeps iv's load when iv is the compute's newest interval so far.
func (c *computeNewest) add(iv waitInterval, aas float64) {
	if c.byClass == nil || iv.to.After(c.to) || (iv.to.Equal(c.to) && iv.from.After(c.from)) {
		c.from, c.to, c.byClass = iv.from, iv.to, map[string]float64{}
	}
	if iv.from.Equal(c.from) && iv.to.Equal(c.to) {
		c.byClass[iv.class] += aas
	}
}

// currentLoad sums each compute's newest interval overlapping newest.
func currentLoad(newest map[string]*computeNewest, points []WaitLoadPoint,
) (map[string]float64, time.Time) {
	out := map[string]float64{}
	if len(points) == 0 {
		return out, time.Time{}
	}
	last := points[len(points)-1]
	for _, c := range newest {
		if !c.to.After(last.From) {
			continue
		}
		for class, v := range c.byClass {
			out[class] += v
		}
	}
	return out, last.T
}

// waitIntervalOf parses one counter event; ok is false for an unusable one
// (another stream, a missing label, a bad interval or a bad number).
func waitIntervalOf(e Event) (waitInterval, bool) {
	f := e.Fields
	if e.Stream != WaitCounters || missing(f["wait_class_name"]) {
		return waitInterval{}, false
	}
	from, err := parseTime(f["previous_ts"])
	if err != nil || !e.OccurredAt.After(from) {
		return waitInterval{}, false
	}
	waited, err := number(f["wait_event_time"])
	if err != nil {
		return waitInterval{}, false
	}
	count := 0.0
	if raw := f["wait_event_count"]; raw != "" {
		if count, err = number(raw); err != nil {
			return waitInterval{}, false
		}
	}
	return waitInterval{from: from, to: e.OccurredAt.UTC(), compute: e.ComputeID,
		class: f["wait_class_name"],
		event: f["wait_event_name"], seconds: waited * WaitEventTimeUnit.Seconds(),
		count: count}, true
}

// WaitLoad groups wait counter events by wait class and event. An interval
// is in the window when it overlaps [from, to]: one starting at from is
// counted, one ending at from is not. It is counted whole, not prorated.
func WaitLoad(events []Event, from, to time.Time) (WaitLoadSeries, error) {
	out := WaitLoadSeries{Points: []WaitLoadPoint{}, Current: map[string]float64{},
		Classes: []string{}, Events: []WaitLoadEvent{}}
	if from.IsZero() || !to.After(from) {
		return out, errors.New("lakebase wait load: until must be after since")
	}
	points := map[[2]time.Time]*WaitLoadPoint{}
	classTotal := map[string]float64{}
	totals := map[[2]string]*WaitLoadEvent{}
	newest := map[string]*computeNewest{}
	for _, e := range events {
		iv, ok := waitIntervalOf(e)
		if !ok {
			out.Skipped++
			continue
		}
		if !iv.from.Before(to) || !iv.to.After(from) {
			continue
		}
		key := [2]time.Time{iv.from, iv.to}
		p := points[key]
		if p == nil {
			p = &WaitLoadPoint{From: iv.from, T: iv.to, ByClass: map[string]float64{}}
			points[key] = p
		}
		aas := iv.seconds / iv.to.Sub(iv.from).Seconds()
		p.ByClass[iv.class] += aas
		p.Sessions += aas
		if newest[iv.compute] == nil {
			newest[iv.compute] = &computeNewest{}
		}
		newest[iv.compute].add(iv, aas)
		classTotal[iv.class] += iv.seconds
		ev := totals[[2]string{iv.class, iv.event}]
		if ev == nil {
			ev = &WaitLoadEvent{WaitClass: iv.class, WaitEvent: iv.event}
			totals[[2]string{iv.class, iv.event}] = ev
		}
		ev.WaitSeconds += iv.seconds
		ev.Count += iv.count
	}
	out.Points = sortedWaitPoints(points)
	out.Current, out.CurrentAt = currentLoad(newest, out.Points)
	out.Classes = classesByTotal(classTotal)
	out.Events = heaviestWaitEvents(totals)
	return out, nil
}

func sortedWaitPoints(points map[[2]time.Time]*WaitLoadPoint) []WaitLoadPoint {
	out := make([]WaitLoadPoint, 0, len(points))
	for _, p := range points {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].T.Equal(out[j].T) {
			return out[i].T.Before(out[j].T)
		}
		return out[i].From.Before(out[j].From)
	})
	return out
}

func classesByTotal(total map[string]float64) []string {
	out := make([]string, 0, len(total))
	for c := range total {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if total[out[i]] != total[out[j]] {
			return total[out[i]] > total[out[j]]
		}
		return out[i] < out[j]
	})
	return out
}

func heaviestWaitEvents(totals map[[2]string]*WaitLoadEvent) []WaitLoadEvent {
	out := make([]WaitLoadEvent, 0, len(totals))
	for _, e := range totals {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].WaitSeconds != out[j].WaitSeconds {
			return out[i].WaitSeconds > out[j].WaitSeconds
		}
		return out[i].WaitClass+"\x00"+out[i].WaitEvent <
			out[j].WaitClass+"\x00"+out[j].WaitEvent
	})
	if len(out) > maxWaitLoadEvents {
		out = out[:maxWaitLoadEvents]
	}
	return out
}
