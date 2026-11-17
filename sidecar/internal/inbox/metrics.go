package inbox

import (
	"sort"
	"strings"
	"sync"
	"time"
)

// The inbox's numbers on /metrics: pg_sage_inbox_items (the waiting
// items by kind at the last complete fleet-wide read) and
// pg_sage_inbox_decision_seconds (approval needed to decision, by kind and
// the surface the decision came from). The process keeps them in memory;
// no table.

// SurfaceHeader names the surface a decision request came from.
const SurfaceHeader = "X-Sage-Surface"

// The surfaces a decision can come from; anything else counts as api.
var surfaces = map[string]bool{"inbox": true, "page": true, "chat": true, "api": true,
	"cli": true}

// Surface maps an X-Sage-Surface value onto a known surface: absent or
// unknown is "api", so a caller cannot mint label values.
func Surface(header string) string {
	s := strings.ToLower(strings.TrimSpace(header))
	if surfaces[s] {
		return s
	}
	return "api"
}

// DecisionBounds are the histogram's upper bounds in seconds: a minute,
// five, fifteen, an hour, four, twelve, a day, three and a week.
var DecisionBounds = []float64{60, 300, 900, 3600, 14400, 43200, 86400, 259200, 604800}

// DecisionSeries is one kind and surface's histogram; Buckets[i] counts
// the decisions at most DecisionBounds[i] (not cumulative).
type DecisionSeries struct {
	Kind    Kind
	Surface string
	Buckets []uint64
	Count   uint64
	Sum     float64
}

type seriesKey struct {
	kind    Kind
	surface string
}

type recorder struct {
	mu        sync.Mutex
	items     map[Kind]int
	decisions map[seriesKey]*DecisionSeries
}

var metrics = &recorder{decisions: map[seriesKey]*DecisionSeries{}}

// RecordDecision counts one decision on an item of kind that had waited
// waited (negative, from clock skew, is zero) from surface.
func RecordDecision(kind Kind, surface string, waited time.Duration) {
	secs := waited.Seconds()
	if secs < 0 {
		secs = 0
	}
	key := seriesKey{kind: kind, surface: Surface(surface)}
	metrics.mu.Lock()
	defer metrics.mu.Unlock()
	s := metrics.decisions[key]
	if s == nil {
		s = &DecisionSeries{Kind: key.kind, Surface: key.surface,
			Buckets: make([]uint64, len(DecisionBounds))}
		metrics.decisions[key] = s
	}
	for i, bound := range DecisionBounds {
		if secs <= bound {
			s.Buckets[i]++
			break
		}
	}
	s.Count++
	s.Sum += secs
}

// ObserveItems records a fleet-wide read's counts for the items gauge. A
// lower bound is not recorded: the gauge never shows a partial count as
// exact.
func ObserveItems(res Result) {
	if res.LowerBound || len(res.CountsByKind) != len(Kinds()) {
		return
	}
	counts := make(map[Kind]int, len(res.CountsByKind))
	for k, n := range res.CountsByKind {
		counts[k] = n
	}
	metrics.mu.Lock()
	metrics.items = counts
	metrics.mu.Unlock()
}

// Snapshot returns the items gauge (nil before any complete read) and
// every decision series, sorted by kind then surface.
func Snapshot() (map[Kind]int, []DecisionSeries) {
	metrics.mu.Lock()
	defer metrics.mu.Unlock()
	var items map[Kind]int
	if metrics.items != nil {
		items = make(map[Kind]int, len(metrics.items))
		for k, n := range metrics.items {
			items[k] = n
		}
	}
	out := make([]DecisionSeries, 0, len(metrics.decisions))
	for _, s := range metrics.decisions {
		c := *s
		c.Buckets = append([]uint64(nil), s.Buckets...)
		out = append(out, c)
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Kind != out[b].Kind {
			return out[a].Kind < out[b].Kind
		}
		return out[a].Surface < out[b].Surface
	})
	return items, out
}
