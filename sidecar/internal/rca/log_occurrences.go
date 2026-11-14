package rca

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// rca-07: a log incident records how often its log line occurred, which
// PIDs logged it (for a deadlock, the participants) and when it was first
// and last seen. The classifier reports a count per dedup window (its
// first signal says 1, a follow-up the window's total), so a window is
// counted once however many of its signals arrive, and windows add up.

// maxLogOccurrencePIDs bounds the PIDs one incident keeps.
const maxLogOccurrencePIDs = 20

// LogOccurrences is the log evidence of an incident, stored in
// sage.incidents.log_occurrences. Values are never mutated after they are
// built: a merge returns a new value, so copies taken for persistence
// outside the engine lock stay consistent.
type LogOccurrences struct {
	Count     int       `json:"count"`
	PIDs      []int     `json:"pids"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
	// Participants is the newest deadlock window's wait cycle (rca-07b),
	// at most MaxDeadlockParticipants; ParticipantsUnparsed counts the
	// DETAIL lines of that window that were not read.
	Participants         []DeadlockParticipant `json:"participants,omitempty"`
	ParticipantsUnparsed int                   `json:"participants_unparsed,omitempty"`
	// windows are the latest classifier windows counted (each one's
	// first_seen and the total it reported), so a later report of any of
	// them adds only its growth. Not stored: after a restart the
	// classifier starts new windows.
	windows []occWindow
}

// occWindow is one classifier dedup window as an incident counted it.
type occWindow struct {
	start time.Time
	count int
}

// maxLogOccurrenceWindows bounds the windows an incident remembers. The
// classifier reports a window only while it is open or right after it
// closes, so a few suffice.
const maxLogOccurrenceWindows = 4

// logOccurrencesFromSignal reads the classifier's occurrence metrics, or
// returns nil when the signal carries no positive occurrence_count.
func logOccurrencesFromSignal(s *Signal) *LogOccurrences {
	if s == nil {
		return nil
	}
	count, ok := s.Metrics["occurrence_count"].(int)
	if !ok || count <= 0 {
		return nil
	}
	occ := &LogOccurrences{Count: count,
		FirstSeen: timeMetric(s, "first_seen"), LastSeen: timeMetric(s, "last_seen")}
	occ.windows = []occWindow{{start: occ.FirstSeen, count: count}}
	if pids, ok := s.Metrics["affected_pids"].([]int); ok {
		occ.PIDs = unionPIDs(nil, pids)
	}
	occ.Participants = participantsFromSignal(s)
	occ.ParticipantsUnparsed, _ = s.Metrics[MetricParticipantsUnparsed].(int)
	return occ
}

// timeMetric parses an RFC 3339 metric, falling back to the signal time.
func timeMetric(s *Signal, key string) time.Time {
	if v, ok := s.Metrics[key].(string); ok {
		if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
			return t
		}
	}
	return s.FiredAt
}

// mergeLogOccurrences folds b into a and returns the result as a new value.
// A report of a window a already counted adds only its growth; another
// window adds its whole count (b may span several windows: one drain can
// carry a new window's first signal and the closed window's follow-up).
// PIDs are unioned (at most 20), first is the earliest sighting and last
// the latest.
func mergeLogOccurrences(a, b *LogOccurrences) *LogOccurrences {
	if b == nil {
		return a
	}
	if a == nil {
		cp := *b
		cp.PIDs = slices.Clone(b.PIDs)
		cp.Participants = slices.Clone(b.Participants)
		cp.windows = slices.Clone(b.windows)
		return &cp
	}
	out := *a
	out.windows = slices.Clone(a.windows)
	if len(b.windows) == 0 {
		out.Count += b.Count
	}
	for _, w := range b.windows {
		out.Count += out.countWindow(w)
	}
	out.PIDs = unionPIDs(a.PIDs, b.PIDs)
	out.mergeParticipants(b)
	if b.FirstSeen.Before(out.FirstSeen) {
		out.FirstSeen = b.FirstSeen
	}
	if b.LastSeen.After(out.LastSeen) {
		out.LastSeen = b.LastSeen
	}
	return &out
}

// countWindow records w and returns how many occurrences it adds: its
// growth over the count already taken for that window, or all of it for
// a window not seen (or no longer remembered). Only the newest
// maxLogOccurrenceWindows windows are kept.
func (o *LogOccurrences) countWindow(w occWindow) int {
	i := slices.IndexFunc(o.windows, func(k occWindow) bool { return k.start.Equal(w.start) })
	if i >= 0 {
		added := max(w.count-o.windows[i].count, 0)
		o.windows[i].count = max(w.count, o.windows[i].count)
		return added
	}
	o.windows = append(o.windows, w)
	slices.SortFunc(o.windows, func(x, y occWindow) int { return x.start.Compare(y.start) })
	if n := len(o.windows); n > maxLogOccurrenceWindows {
		o.windows = o.windows[n-maxLogOccurrenceWindows:]
	}
	return w.count
}

// mergeParticipants keeps the newest window's wait cycle (rca-07b): b's
// set replaces o's unless b has none or was last seen before o (an older
// set only fills an empty one); the unparsed count follows the same rule.
// o is a fresh copy (mergeLogOccurrences), and the set is cloned, so the
// result never aliases b.
func (o *LogOccurrences) mergeParticipants(b *LogOccurrences) {
	newer := !b.LastSeen.Before(o.LastSeen)
	if len(b.Participants) > 0 && (newer || len(o.Participants) == 0) {
		o.Participants = slices.Clone(b.Participants[:min(len(b.Participants),
			MaxDeadlockParticipants)])
	}
	if b.ParticipantsUnparsed > 0 && (newer || o.ParticipantsUnparsed == 0) {
		o.ParticipantsUnparsed = b.ParticipantsUnparsed
	}
}

// unionPIDs returns the sorted union of a and b, capped at 20 PIDs (the
// ones already held first).
func unionPIDs(a, b []int) []int {
	out := slices.Clone(a)
	for _, pid := range b {
		if len(out) >= maxLogOccurrencePIDs {
			break
		}
		if !slices.Contains(out, pid) {
			out = append(out, pid)
		}
	}
	slices.Sort(out)
	return out
}

// attachLogOccurrences sets each single-signal log incident's occurrences
// from every signal of its id in the batch, so an immediate signal and its
// follow-up count their window once.
func attachLogOccurrences(incidents []Incident, signals []*Signal) {
	for i := range incidents {
		if len(incidents[i].SignalIDs) != 1 {
			continue
		}
		id := incidents[i].SignalIDs[0]
		var occ *LogOccurrences
		for _, s := range signals {
			if s != nil && s.ID == id {
				occ = mergeLogOccurrences(occ, logOccurrencesFromSignal(s))
			}
		}
		incidents[i].LogOccurrences = occ
	}
}

const writeLogOccurrencesSQL = `/* pg_sage */
UPDATE sage.incidents SET log_occurrences = $2::jsonb
WHERE id = $1 AND log_occurrences IS DISTINCT FROM $2::jsonb`

// writeLogOccurrences stores inc's occurrences in a statement of its own,
// so a failure here never loses the incident row written before it.
func writeLogOccurrences(ctx context.Context, pool *pgxpool.Pool, inc *Incident) error {
	raw, err := json.Marshal(inc.LogOccurrences)
	if err != nil {
		return fmt.Errorf("marshal log_occurrences: %w", err)
	}
	if _, err := pool.Exec(ctx, writeLogOccurrencesSQL, inc.ID, string(raw)); err != nil {
		return fmt.Errorf("update log_occurrences: %w", err)
	}
	return nil
}

// persistedFingerprint writes the incident's occurrences (if any) and
// returns the fingerprint of what is now stored, or "" when the write
// failed: the row is then written again next pass, which retries it. The
// failure is logged with the incident id; the incident row stays.
func (e *Engine) persistedFingerprint(
	ctx context.Context, pool *pgxpool.Pool, inc *Incident,
) string {
	if inc.LogOccurrences != nil {
		if err := writeLogOccurrences(ctx, pool, inc); err != nil {
			e.logFn("warn", "rca: incident %s stored without its log_occurrences "+
				"(retried next pass): %v", inc.ID, err)
			return ""
		}
	}
	return storedFingerprint(inc)
}

// hydrateLogOccurrences decodes a stored log_occurrences value.
func hydrateLogOccurrences(inc *Incident, raw []byte) error {
	if len(raw) == 0 {
		return nil
	}
	var occ LogOccurrences
	if err := json.Unmarshal(raw, &occ); err != nil {
		return fmt.Errorf("rca: hydrate log_occurrences of %s: %w", inc.ID, err)
	}
	inc.LogOccurrences = &occ
	return nil
}
