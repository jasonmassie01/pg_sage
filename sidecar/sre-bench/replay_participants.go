package srebench

import (
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/logwatch"
	"github.com/pg-sage/sidecar/sre-bench/replay"
)

// rca-07b: a deadlock replay case scores participants_named, whether
// pg_sage's log parser names every expected participant of its server-log
// record with its lock mode, its blocker and a statement fingerprint. The
// score depends on the parser, not on an arm, so it is reported once per
// case.

// ParticipantsScore is one deadlock case's participants_named score.
type ParticipantsScore struct {
	Case  string `json:"case"`
	Named int    `json:"participants_named"`
}

// participantsScores scores every case that carries a deadlock record, in
// case order; cases without one are not scored.
func participantsScores(cases []replay.Case) []ParticipantsScore {
	var out []ParticipantsScore
	for _, c := range cases {
		if named, ok := replay.ParticipantsNamed(c, logwatchDeadlocks); ok {
			out = append(out, ParticipantsScore{Case: c.ID, Named: named})
		}
	}
	return out
}

// writeParticipants renders the participants_named scores, if any.
func writeParticipants(b *strings.Builder, scores []ParticipantsScore) {
	if len(scores) == 0 {
		return
	}
	b.WriteString("\n### Deadlock participants named\n\n")
	b.WriteString(row("case", "participants_named"))
	b.WriteString(separator(2))
	for _, s := range scores {
		b.WriteString(row(s.Case, fmt.Sprint(s.Named)))
	}
}

// logwatchDeadlocks is pg_sage's deadlock log parser as the replay's
// DeadlockParser.
func logwatchDeadlocks(detail, context string) []replay.ParsedParticipant {
	parts, _ := logwatch.ParseDeadlockDetail(detail, context)
	out := make([]replay.ParsedParticipant, 0, len(parts))
	for _, p := range parts {
		out = append(out, replay.ParsedParticipant{PID: p.PID, BlockedBy: p.BlockedBy,
			WaitMode: p.WaitMode, Fingerprint: p.Fingerprint})
	}
	return out
}
