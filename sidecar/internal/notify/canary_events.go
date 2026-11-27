package notify

import (
	"fmt"
	"strings"
)

// CanaryTargetAction is a database a fleet canary was changing when it
// stopped, and the action it ran there (0: not yet changed).
type CanaryTargetAction struct {
	Database    string
	ActionLogID int64
}

// CanaryInterrupted is a fleet canary a sidecar restart interrupted
// (executor-04b): pg_sage does not resume it, so an operator decides.
// Changing are the databases in flight; Kept are those it had changed
// with a verdict recorded, which keep the change (nothing rolls back).
type CanaryInterrupted struct {
	RunID    string
	Source   string
	Changing []CanaryTargetAction
	Kept     []CanaryTargetAction
}

// CanaryInterruptedEvent is the one warning event of an interrupted
// canary. Its dedup key (canary_interrupted:<run id>) makes a repeat a
// PagerDuty no-op.
func CanaryInterruptedEvent(info CanaryInterrupted) Event {
	changing, inFlight := canaryTargetLines(info.Changing)
	kept, keptLines := canaryTargetLines(info.Kept)
	body := "No database was being changed."
	if len(inFlight) > 0 {
		body = "In flight: " + strings.Join(inFlight, ", ") + ". A change already made " +
			"stays with that database's own post-action monitor: check its action log " +
			"and keep or roll it back."
	}
	if len(keptLines) > 0 {
		body += "\nChanged with a verdict, kept (nothing was rolled back): " +
			strings.Join(keptLines, ", ") + "."
	}
	return Event{
		Type:     "canary_interrupted",
		Severity: "warning",
		Subject: fmt.Sprintf("Fleet canary %s from %s interrupted by a sidecar restart",
			info.RunID, info.Source),
		Body: fmt.Sprintf("Run: %s\nSource database: %s\n%s\nThe canary is not resumed; "+
			"start a new one if the change should still roll out.",
			info.RunID, info.Source, body),
		Data: map[string]any{
			"run_id": info.RunID, "source_database": info.Source, "changing": changing,
			"kept": kept,
		},
		DedupKey: "canary_interrupted:" + info.RunID,
	}
}

// canaryTargetLines are the targets' names and their "name (action N)"
// lines.
func canaryTargetLines(targets []CanaryTargetAction) (names, lines []string) {
	names = make([]string, 0, len(targets))
	lines = make([]string, 0, len(targets))
	for _, c := range targets {
		names = append(names, c.Database)
		if c.ActionLogID > 0 {
			lines = append(lines, fmt.Sprintf("%s (action %d)", c.Database, c.ActionLogID))
		} else {
			lines = append(lines, c.Database+" (no action recorded yet)")
		}
	}
	return names, lines
}
