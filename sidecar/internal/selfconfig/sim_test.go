package selfconfig

import (
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// No concurrent access tests in the pure stepping tests: Step is a pure
// function of its input, and the engine runs one pass per database under
// an advisory lock (store_db_test.go covers concurrent passes).

// simulation is one key stepped through hourly live passes the way the
// engine steps it (Evaluate, Observe, Step, apply), without a database.
type simulation struct {
	rule  Rule
	cfg   *config.Config
	state State
	now   time.Time
	// events is every ledger row the passes produced, in order.
	events []Event
	// promotedAt is the pass index of the first promotion (-1: none).
	promotedAt int
	passes     int
}

func newSimulation(r Rule, cfg *config.Config) *simulation {
	return &simulation{rule: r, cfg: config.Clone(cfg), now: t0, promotedAt: -1}
}

// pass steps once on ev, one hour after the previous pass.
func (s *simulation) pass(ev Evidence) StepResult {
	in := StepInput{Rule: s.rule, Restart: isRestart(s.rule.Key), Phase: PhaseLive,
		Now: s.now, Soak: soak, Prev: s.state, Proposal: s.rule.Evaluate(ev, s.cfg),
		Running: s.rule.Get(s.cfg), Default: s.rule.Get(s.cfg), Cfg: s.cfg}
	if s.rule.Observe != nil {
		if v, ok := s.rule.Observe(ev, s.cfg); ok {
			in.Sample = &v
		}
	}
	res := Step(in)
	if res.Apply {
		s.rule.Set(s.cfg, res.Next.Value)
	}
	for _, e := range res.Events {
		if e.Kind == EventPromoted && s.promotedAt < 0 {
			s.promotedAt = s.passes
		}
	}
	s.events = append(s.events, res.Events...)
	s.state = res.Next
	s.now = s.now.Add(time.Hour)
	s.passes++
	return res
}

// run steps n passes, evidence ev(i) on pass i.
func (s *simulation) run(n int, ev func(i int) Evidence) {
	for i := 0; i < n; i++ {
		s.pass(ev(i))
	}
}

func (s *simulation) count(kind EventKind) int {
	n := 0
	for _, e := range s.events {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

// soakPasses is how many hourly passes one soak spans.
const soakPasses = int(soak / time.Hour)

// costEvidence is evidence on which a cost-like rule derives raw.
func costEvidence(t *testing.T, key string, raw float64) Evidence {
	t.Helper()
	switch key {
	case "collector.interval_seconds":
		return Evidence{CollectorCycleMs: Known(raw * 1000 * collectorBudget)}
	case "safety.query_timeout_ms":
		return Evidence{CatalogScanMs: Known(raw / timeoutHeadroom)}
	case "sre.runways.sequence_interval_seconds":
		return Evidence{SequenceScanMs: Known(raw * 1000 * sequenceBudget)}
	}
	t.Fatalf("no cost evidence for %s", key)
	return Evidence{}
}
