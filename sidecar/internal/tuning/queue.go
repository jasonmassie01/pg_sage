package tuning

import (
	"sort"
	"strings"
	"sync"

	"github.com/pg-sage/sidecar/internal/analyzer"
)

// maxDeferredLogged bounds the case ids one deferral log line names.
const maxDeferredLogged = 10

// caseQueue remembers, per case, how many cycles in a row it was left for
// later, so the next cycle asks the longest-waiting cases first and the
// most valuable among equals (DetectCases order).
type caseQueue struct {
	age map[string]int
}

// order is the cases, longest waiting first, otherwise in value order.
func (q *caseQueue) order(cases []Case) []Case {
	out := append([]Case(nil), cases...)
	sort.SliceStable(out, func(i, j int) bool { return q.age[out[i].ID] > q.age[out[j].ID] })
	return out
}

// advance ages the deferred cases; every other case starts over.
func (q *caseQueue) advance(deferred []string) {
	next := make(map[string]int, len(deferred))
	for _, id := range deferred {
		next[id] = q.age[id] + 1
	}
	q.age = next
}

func (a *Agent) logDeferred(deferred []string, budget bool) {
	if len(deferred) == 0 {
		return
	}
	why := "case cap"
	if budget {
		why = "budget exhausted or model unavailable"
	}
	ids := deferred
	more := ""
	if len(ids) > maxDeferredLogged {
		ids, more = ids[:maxDeferredLogged], ", ..."
	}
	a.logFn("INFO", "tuning: %d case(s) deferred to a later cycle (%s): %s%s",
		len(deferred), why, strings.Join(ids, ", "), more)
}

// cycleStats is the last cycle's budget use, read by Stats.
type cycleStats struct {
	mu sync.Mutex
	s  analyzer.TuningStats
}

// noteCycle records the cycle's budget use and case counts.
func (a *Agent) noteCycle(b *CycleBudget, asked, deferred int) {
	t := a.settings.Tuning
	reqs, tokens := b.Used()
	a.last.mu.Lock()
	defer a.last.mu.Unlock()
	a.last.s = analyzer.TuningStats{TokensUsed: tokens,
		TokenLimit: int64(t.MaxTokensPerCycle), RequestsUsed: int64(reqs),
		RequestLimit: int64(t.MaxRequestsPerCycle), CasesAsked: int64(asked),
		CasesDeferred: int64(deferred)}
}
