package replay

import (
	"strings"
)

// rca-07b: a deadlock case can carry the deadlock's server-log record
// (DETAIL and CONTEXT, as csvlog writes them) and the participants a DBA
// needs named: who waited for which lock mode, blocked by whom. The replay
// scores participants_named by running the record through the log
// parser pg_sage uses.

// DeadlockLog is a case's deadlock record and its expected participants.
type DeadlockLog struct {
	Detail       string                `json:"detail"`
	Context      string                `json:"context,omitempty"`
	Participants []ExpectedParticipant `json:"participants"`
}

// ExpectedParticipant is one process the parser must name.
type ExpectedParticipant struct {
	PID       int    `json:"pid"`
	WaitMode  string `json:"wait_mode"`
	BlockedBy int    `json:"blocked_by"`
}

// validateDeadlockLog checks an optional deadlock record: a DETAIL and at
// least one expected participant, each with a PID, a lock mode and a
// blocker.
func (c Case) validateDeadlockLog() error {
	d := c.DeadlockLog
	if d == nil {
		return nil
	}
	if strings.TrimSpace(d.Detail) == "" {
		return invalid("deadlock_log has no detail")
	}
	if len(d.Participants) == 0 {
		return invalid("deadlock_log names no expected participants")
	}
	for i, p := range d.Participants {
		if p.PID <= 0 || p.BlockedBy <= 0 || p.WaitMode == "" {
			return invalid("deadlock_log participant %d needs pid, wait_mode and "+
				"blocked_by: %+v", i, p)
		}
	}
	return nil
}

// ParsedParticipant is one process a deadlock parser named.
type ParsedParticipant struct {
	PID, BlockedBy        int
	WaitMode, Fingerprint string
}

// DeadlockParser reads a deadlock record's DETAIL and CONTEXT; the bench
// passes pg_sage's log parser (logwatch.ParseDeadlockDetail). It is a
// parameter so this package, which the SRE package's tests load, does not
// import logwatch, which imports rca and so sre (an import cycle).
type DeadlockParser func(detail, context string) []ParsedParticipant

// ParticipantsNamed scores a deadlock case: 1 when parse names every
// expected participant with its lock mode, its blocker and a statement
// fingerprint, else 0 (also with no parser). ok is false for a case
// without a deadlock record (not scored).
func ParticipantsNamed(c Case, parse DeadlockParser) (named int, ok bool) {
	if c.DeadlockLog == nil {
		return 0, false
	}
	if parse == nil {
		return 0, true
	}
	parts := parse(c.DeadlockLog.Detail, c.DeadlockLog.Context)
	got := make(map[int]ParsedParticipant, len(parts))
	for _, p := range parts {
		got[p.PID] = p
	}
	for _, want := range c.DeadlockLog.Participants {
		p, found := got[want.PID]
		if !found || p.WaitMode != want.WaitMode || p.BlockedBy != want.BlockedBy ||
			p.Fingerprint == "" {
			return 0, true
		}
	}
	return 1, true
}
