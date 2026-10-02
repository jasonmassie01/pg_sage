// Package earned is Sage SRE's earned-autonomy ledger (AI-SRE-SPEC §7.3,
// M7): a durable level per incident family x action class, promoted only
// from benchmark, shadow and live-recovery evidence with a human approval,
// and downgraded automatically when the error budget burns, a failover is
// in progress, evidence is stale, another pg_sage action touches the same
// object or the family regressed on safety. The standing policy gate
// consults it; the operator's own trust settings stay the outer bound.
package earned

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Level is an autonomy level.
type Level int

// Autonomy levels.
const (
	// L0 is an evidence packet only.
	L0 Level = iota
	// L1 adds hypotheses and a proposal as a manual script.
	L1
	// L2 adds a one-click approval handoff.
	L2
	// L3 auto-executes reversible, bounded actions inside the window and
	// notifies a human.
	L3
	// L4 (auto-execute and auto-rollback without a window) is reserved
	// and can never be granted.
	L4
)

// MaxGrantable is the highest level a human can approve.
const MaxGrantable = L3

// String is the level's label, "L0".."L4", or "L?" out of range.
func (l Level) String() string {
	if l < L0 || l > L4 {
		return "L?"
	}
	return "L" + strconv.Itoa(int(l))
}

// Grantable reports whether l may be stored in the ledger (L0..L3).
func (l Level) Grantable() bool { return l >= L0 && l <= MaxGrantable }

// ParseLevel reads "L2", "l2" or "2" (L0..L4).
func ParseLevel(s string) (Level, error) {
	v := strings.ToUpper(strings.TrimSpace(s))
	v = strings.TrimPrefix(v, "L")
	if len(v) != 1 || v[0] < '0' || v[0] > '4' {
		return 0, fmt.Errorf("%w: level %q is not L0..L4", ErrInvalidRequest, s)
	}
	return Level(v[0] - '0'), nil
}

// MarshalJSON renders the label.
func (l Level) MarshalJSON() ([]byte, error) { return json.Marshal(l.String()) }

// UnmarshalJSON reads a label or digit.
func (l *Level) UnmarshalJSON(raw []byte) error {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		var n int
		if err := json.Unmarshal(raw, &n); err != nil {
			return fmt.Errorf("level: %w", err)
		}
		s = strconv.Itoa(n)
	}
	parsed, err := ParseLevel(s)
	if err != nil {
		return err
	}
	*l = parsed
	return nil
}

// MinLevel is the lowest of levels; no level at all is L0.
func MinLevel(levels ...Level) Level {
	if len(levels) == 0 {
		return L0
	}
	out := levels[0]
	for _, l := range levels[1:] {
		out = min(out, l)
	}
	return out
}
