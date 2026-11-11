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
type CanaryInterrupted struct {
	RunID    string
	Source   string
	Changing []CanaryTargetAction
}

// CanaryInterruptedEvent is the one warning event of an interrupted
// canary. Its dedup key (canary_interrupted:<run id>) makes a repeat a
// PagerDuty no-op.
func CanaryInterruptedEvent(info CanaryInterrupted) Event {
	names := make([]string, 0, len(info.Changing))
	lines := make([]string, 0, len(info.Changing))
	for _, c := range info.Changing {
		names = append(names, c.Database)
		if c.ActionLogID > 0 {
			lines = append(lines, fmt.Sprintf("%s (action %d)", c.Database, c.ActionLogID))
		} else {
			lines = append(lines, c.Database+" (no action recorded yet)")
		}
	}
	inFlight := "No database was being changed."
	if len(lines) > 0 {
		inFlight = "In flight: " + strings.Join(lines, ", ") + ". A change already made " +
			"stays with that database's own post-action monitor: check its action log " +
			"and keep or roll it back."
	}
	return Event{
		Type:     "canary_interrupted",
		Severity: "warning",
		Subject: fmt.Sprintf("Fleet canary %s from %s interrupted by a sidecar restart",
			info.RunID, info.Source),
		Body: fmt.Sprintf("Run: %s\nSource database: %s\n%s\nThe canary is not resumed; "+
			"start a new one if the change should still roll out.",
			info.RunID, info.Source, inFlight),
		Data: map[string]any{
			"run_id": info.RunID, "source_database": info.Source, "changing": names,
		},
		DedupKey: "canary_interrupted:" + info.RunID,
	}
}
