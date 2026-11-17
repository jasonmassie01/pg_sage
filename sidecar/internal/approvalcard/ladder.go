package approvalcard

import (
	"log/slog"

	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/verify"
)

// LadderNoneRecorded is the card's note when its queue row holds no
// verification ladder (queued before lab-03a, or nothing recorded).
const LadderNoneRecorded = "No pre-production test recorded"

// ladderOf is the card's ladder section: the queue row's records, or the
// none-recorded note for a NULL or empty ladder. A stored ladder that is
// not an array of records gives neither and one WARN; the card renders.
func ladderOf(in Inputs) ([]verify.LadderRecord, string) {
	recs, err := verify.ParseLadder(in.Ladder)
	if err != nil {
		slog.Warn("approval card: the queue item's verification ladder is unreadable; "+
			"the card shows no ladder (check sage.action_queue.ladder)",
			"queue_id", in.Action.ID, "error", err)
		return nil, ""
	}
	if len(recs) == 0 {
		return nil, LadderNoneRecorded
	}
	return recs, ""
}

// notRehearsedReason says a costly-to-revert action waits because its
// ladder has no passed clone test (the executor's entry rule).
func notRehearsedReason(in Inputs, actionType string) (Reason, bool) {
	recs, _ := verify.ParseLadder(in.Ladder)
	text := executor.NotRehearsedReason(actionType, in.Action.ProposedSQL, recs)
	if text == "" || actionType == "" {
		return Reason{}, false
	}
	return Reason{Code: "not_rehearsed", Text: "This change is " + text}, true
}
