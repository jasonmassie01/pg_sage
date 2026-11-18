package modelpass

import (
	"fmt"
	"sync"

	"github.com/pg-sage/sidecar/internal/review/report"
)

// moves are the model pass's legal transitions: pending to running (or
// straight to skipped or failed when it never starts), running to done,
// failed or skipped. done, failed and skipped are final.
var moves = map[report.PassStatus]map[report.PassStatus]bool{
	report.PassPending: {report.PassRunning: true, report.PassSkipped: true,
		report.PassFailed: true},
	report.PassRunning: {report.PassDone: true, report.PassFailed: true,
		report.PassSkipped: true},
}

// tracker holds one pass's state and its history.
type tracker struct {
	mu      sync.Mutex
	history []report.PassStatus
	onState func(report.PassStatus)
}

func newTracker() *tracker {
	return &tracker{history: []report.PassStatus{report.PassPending}}
}

// Status is the current state.
func (t *tracker) Status() report.PassStatus {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.history[len(t.history)-1]
}

// History is every state entered, in order.
func (t *tracker) History() []report.PassStatus {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]report.PassStatus(nil), t.history...)
}

func (t *tracker) advance(to report.PassStatus) error {
	t.mu.Lock()
	from := t.history[len(t.history)-1]
	if !moves[from][to] {
		t.mu.Unlock()
		return fmt.Errorf("review model pass: no move from %s to %s", from, to)
	}
	t.history = append(t.history, to)
	t.mu.Unlock()
	t.notify(to)
	return nil
}

func (t *tracker) notify(s report.PassStatus) {
	if t.onState != nil {
		t.onState(s)
	}
}
