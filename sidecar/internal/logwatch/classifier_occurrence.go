package logwatch

import (
	"maps"
	"regexp"
	"slices"
	"strconv"
	"time"

	"github.com/pg-sage/sidecar/internal/rca"
	"github.com/pg-sage/sidecar/internal/signalid"
)

// rca-07: the dedup window used to throw its counts away. Each entry now
// keeps counting, and a drain reports the repeats of every entry in one
// follow-up signal of the same id carrying occurrence_count,
// affected_pids (at most MaxAffectedPIDs), first_seen and last_seen. The
// first occurrence is still emitted at once, with a count of 1. A
// follow-up's count is the window's total, so a consumer that sees both
// the first signal and a follow-up of one window counts the window once.

// reDeadlockProcess finds the participant PIDs of a deadlock DETAIL
// ("Process 5678 waits for ShareLock ...; blocked by process 5679."). The
// wait cycle and statements are read by ParseDeadlockDetail (rca-07b).
var reDeadlockProcess = regexp.MustCompile(`(?i)\bprocess (\d+)\b`)

// occurrencePIDs are the PIDs one log line counts: the logging backend
// and, for a deadlock, every participant its DETAIL names.
func occurrencePIDs(signalID string, entry LogEntry) []int {
	pids := []int{entry.PID}
	if signalID != signalid.LogDeadlockDetected || entry.Detail == "" {
		return pids
	}
	for _, m := range reDeadlockProcess.FindAllStringSubmatch(entry.Detail, -1) {
		if pid, err := strconv.Atoi(m[1]); err == nil && !slices.Contains(pids, pid) {
			pids = append(pids, pid)
		}
	}
	return pids
}

// recordOccurrence counts one occurrence of key at ts. It returns true when
// the occurrence falls in an open window (a duplicate). Otherwise it opens
// a window with sig as its first signal, stamped with a count of 1; the
// window it replaces leaves a pending follow-up if it had unreported
// repeats. An event exactly one window after the first starts a new one.
// Caller holds c.mu.
func (c *Classifier) recordOccurrence(
	key string, pids []int, ts time.Time, sig *rca.Signal,
) bool {
	de, exists := c.dedupBuf[key]
	if exists && ts.Sub(de.firstSeen) < c.dedupWindow {
		de.count++
		if ts.After(de.lastSeen) {
			de.lastSeen = ts
		}
		addPIDs(de.pids, pids)
		de.noteParticipants(sig)
		return true
	}
	if exists {
		c.closeEntry(de)
	}
	de = &dedupEntry{firstSeen: ts, lastSeen: ts, count: 1, emitted: 1,
		pids: make(map[int]bool, len(pids))}
	addPIDs(de.pids, pids)
	de.noteParticipants(sig)
	stampOccurrences(sig, de)
	// A private copy: consumers own the returned signal and its metrics.
	de.sig = &rca.Signal{ID: sig.ID, FiredAt: sig.FiredAt, Severity: sig.Severity,
		Metrics: maps.Clone(sig.Metrics)}
	c.dedupBuf[key] = de
	return false
}

// addPIDs adds pids to set until it holds MaxAffectedPIDs.
func addPIDs(set map[int]bool, pids []int) {
	for _, pid := range pids {
		if len(set) >= MaxAffectedPIDs {
			return
		}
		set[pid] = true
	}
}

// closeEntry queues the follow-up of a window that is going away with
// repeats no signal reported. Caller holds c.mu.
func (c *Classifier) closeEntry(de *dedupEntry) {
	if up := followUp(de); up != nil {
		c.pending = append(c.pending, up)
	}
}

// FollowUps returns one signal per dedup window that counted occurrences
// since a signal last reported it, closed windows first, and marks them
// reported. The drain calls it before CleanExpiredDedup.
func (c *Classifier) FollowUps() []*rca.Signal {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.pending
	c.pending = nil
	for _, key := range slices.Sorted(maps.Keys(c.dedupBuf)) {
		if up := followUp(c.dedupBuf[key]); up != nil {
			out = append(out, up)
		}
	}
	return out
}

// followUp builds the signal reporting de's repeats and marks them
// reported, or returns nil when there is nothing new (or no template).
func followUp(de *dedupEntry) *rca.Signal {
	if de == nil || de.sig == nil || de.count <= de.emitted {
		return nil
	}
	de.emitted = de.count
	up := &rca.Signal{ID: de.sig.ID, FiredAt: de.lastSeen, Severity: de.sig.Severity,
		Metrics: maps.Clone(de.sig.Metrics)}
	stampOccurrences(up, de)
	return up
}

// stampOccurrences writes de's counts into sig's metrics.
func stampOccurrences(sig *rca.Signal, de *dedupEntry) {
	sig.Metrics["occurrence_count"] = de.count
	sig.Metrics["affected_pids"] = slices.Sorted(maps.Keys(de.pids))
	sig.Metrics["first_seen"] = de.firstSeen.Format(time.RFC3339Nano)
	sig.Metrics["last_seen"] = de.lastSeen.Format(time.RFC3339Nano)
	if len(de.participants) > 0 {
		sig.Metrics[rca.MetricDeadlockParticipants] = de.participants
	}
	if de.unparsed > 0 {
		sig.Metrics[rca.MetricParticipantsUnparsed] = de.unparsed
	}
}

// noteParticipants records the wait cycle of the line sig was built from
// (rca-07b): the newest set the window saw replaces the last one, and the
// unread DETAIL lines add up over the window. A set is never mutated
// after it is built, so signals may share it.
func (de *dedupEntry) noteParticipants(sig *rca.Signal) {
	if parts, ok := sig.Metrics[rca.MetricDeadlockParticipants].([]rca.DeadlockParticipant); ok &&
		len(parts) > 0 {
		de.participants = parts
	}
	if n, ok := sig.Metrics[rca.MetricParticipantsUnparsed].(int); ok {
		de.unparsed += n
	}
}

// addDeadlockParticipants parses a deadlock's DETAIL into its wait cycle
// (rca-07b). Statement text is dropped unless redacted mode keeps it; a
// DETAIL that names nobody leaves the PID-only signal of rca-07.
func (c *Classifier) addDeadlockParticipants(entry LogEntry, metrics map[string]any) {
	parts, unparsed := ParseDeadlockDetail(entry.Detail, entry.Context)
	if !c.redactedStatements {
		for i := range parts {
			parts[i].Statement = ""
		}
	}
	if len(parts) > 0 {
		metrics[rca.MetricDeadlockParticipants] = parts
	}
	if unparsed > 0 {
		metrics[rca.MetricParticipantsUnparsed] = unparsed
	}
}
