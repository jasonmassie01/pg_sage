package rca

import (
	"fmt"
	"slices"
	"strings"
)

// rca-07b: a deadlock incident names its wait cycle. The log classifier
// parses the deadlock DETAIL into participants: which lock each process
// waited for, which process blocked it, and a fingerprint of the
// statement it ran. Statement text is kept only when
// rca.deadlock_statement_text is "redacted", and then only literal-redacted.

// Signal metric keys the classifier sets on log_deadlock_detected.
const (
	// MetricDeadlockParticipants holds a []DeadlockParticipant.
	MetricDeadlockParticipants = "deadlock_participants"
	// MetricParticipantsUnparsed counts DETAIL lines that were not read
	// (an unknown shape or a non-English lc_messages).
	MetricParticipantsUnparsed = "participants_unparsed"
)

// MaxDeadlockParticipants bounds the participants one deadlock keeps.
const MaxDeadlockParticipants = 10

// DeadlockParticipant is one process of a deadlock as its DETAIL names it.
// WaitObject keeps PostgreSQL's wording, OIDs included ("transaction 745",
// "tuple (0,1) of relation 16384 of database 5"). Relation is the relation
// the CONTEXT names, set on the detecting backend only (the first DETAIL
// line). Fingerprint is the first 16 hex characters of sha256 over the
// literal-redacted statement; Statement is that redacted text, empty
// (absent in JSON) unless redacted mode keeps it.
type DeadlockParticipant struct {
	PID         int    `json:"pid"`
	WaitMode    string `json:"wait_mode,omitempty"`
	WaitObject  string `json:"wait_object,omitempty"`
	BlockedBy   int    `json:"blocked_by,omitempty"`
	Relation    string `json:"relation,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
	Statement   string `json:"statement,omitempty"`
}

// EvidenceLine is the participant's wait as an operator reads it:
// "pid A waits for MODE on OBJECT (relation "r") held by pid B".
func (p DeadlockParticipant) EvidenceLine() string {
	var b strings.Builder
	fmt.Fprintf(&b, "pid %d", p.PID)
	if p.WaitMode != "" {
		fmt.Fprintf(&b, " waits for %s on %s", p.WaitMode, p.WaitObject)
		if p.Relation != "" {
			fmt.Fprintf(&b, " (relation %q)", p.Relation)
		}
		if p.BlockedBy != 0 {
			fmt.Fprintf(&b, " held by pid %d", p.BlockedBy)
		}
	}
	if p.Fingerprint != "" {
		b.WriteString("; statement fp=" + p.Fingerprint)
	}
	return b.String()
}

// participantsFromSignal reads the classifier's participants metric, at
// most MaxDeadlockParticipants of them, as a private copy. A metric of
// another type is ignored.
func participantsFromSignal(s *Signal) []DeadlockParticipant {
	parts, ok := s.Metrics[MetricDeadlockParticipants].([]DeadlockParticipant)
	if !ok || len(parts) == 0 {
		return nil
	}
	return slices.Clone(parts[:min(len(parts), MaxDeadlockParticipants)])
}

// deadlockCycleLink is the chain link naming every participant's wait, or
// false when the signal carries none.
func deadlockCycleLink(s *Signal) (ChainLink, bool) {
	parts := participantsFromSignal(s)
	if len(parts) == 0 {
		return ChainLink{}, false
	}
	lines := make([]string, len(parts))
	for i, p := range parts {
		lines[i] = p.EvidenceLine()
	}
	return ChainLink{Order: 2, Signal: s.ID,
		Description: "Deadlock wait cycle: each process, the lock it waited for and its holder",
		Evidence:    strings.Join(lines, "\n")}, true
}
