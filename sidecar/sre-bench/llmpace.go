package srebench

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Pacing the LLM-on arm's live model: PG_SAGE_BENCH_LLM_RPM spaces every
// call the arm makes, across all of its runs (fault programs and
// replay), at most that many per minute. A provider's rate limit then
// does not turn a measurement into a run of fallbacks.

// EnvLLMRPM caps the live model's calls per minute (1-6000; unset:
// unpaced).
const EnvLLMRPM = "PG_SAGE_BENCH_LLM_RPM"

const maxRPM = 6000

// pacer hands out call slots at least interval apart.
type pacer struct {
	interval time.Duration
	mu       sync.Mutex
	next     time.Time
}

func newPacer(rpm int) *pacer { return &pacer{interval: time.Minute / time.Duration(rpm)} }

// wait blocks until the caller's slot; a nil pacer never waits.
func (p *pacer) wait(ctx context.Context) error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	now := time.Now()
	slot := p.next
	if slot.Before(now) {
		slot = now
	}
	p.next = slot.Add(p.interval)
	p.mu.Unlock()
	t := time.NewTimer(time.Until(slot))
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// parseRPM reads EnvLLMRPM; 0 means unpaced.
func parseRPM(raw string) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > maxRPM {
		return 0, fmt.Errorf("%s=%q: want an integer from 1 to %d", EnvLLMRPM, raw, maxRPM)
	}
	return n, nil
}
