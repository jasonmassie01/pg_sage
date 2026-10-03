package retention

import (
	"sync"
	"time"

	"github.com/pg-sage/sidecar/internal/partition"
)

// capUnder is the size cap state of a table that fits its cap.
const capUnder = "under"

// capNotes rate-limits the size cap's log lines. Retention runs every few
// minutes; a table over its cap for days logged the same warning on every
// run. A line is now logged when a table's cap state changes, and repeated
// at most once per UTC day while it lasts. Being under the cap is logged
// only when a table comes back under it. Safe for concurrent use.
type capNotes struct {
	mu   sync.Mutex
	last map[string]capNote
}

type capNote struct {
	state string
	day   time.Time
}

// due reports whether table's state at now is worth a log line, and
// records it. A nil set logs everything.
func (n *capNotes) due(table, state string, now time.Time) bool {
	if n == nil {
		return true
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.last == nil {
		n.last = map[string]capNote{}
	}
	prev, seen := n.last[table]
	day := partition.DayStart(now)
	n.last[table] = capNote{state: state, day: day}
	if state == capUnder {
		return seen && prev.state != capUnder
	}
	return !seen || prev.state != state || !prev.day.Equal(day)
}

// note logs a size cap line for table when its state calls for one.
func (c *Cleaner) note(table, state, level, format string, args ...any) {
	if c.notes.due(table, state, time.Now()) {
		c.logFn(level, format, args...)
	}
}
