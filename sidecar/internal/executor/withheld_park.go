package executor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/analyzer"
)

// withheldContentKey is the before_state key that marks a content-bound
// withhold, and the hash of the content the withhold depends on.
const withheldContentKey = "withheld_content"

// contentBoundWithhold reports a CREATE INDEX that verified-index
// admission refuses for its own content alone (no index name, no or a
// mismatched rollback, IF NOT EXISTS, UNIQUE, no target queries): it will
// be refused again on every cycle until that content changes.
func contentBoundWithhold(f analyzer.Finding) bool {
	if categorizeAction(f.RecommendedSQL) != "create_index" {
		return false
	}
	_, err := verifiedActionForFinding(f)
	return err != nil
}

// withheldContent hashes what contentBoundWithhold depends on: the
// forward SQL, the rollback and the target queries.
func withheldContent(f analyzer.Finding) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(f.RecommendedSQL) + "\x00" +
		strings.TrimSpace(f.RollbackSQL) + "\x00" + fmt.Sprint(targetQueryIDs(f))))
	return hex.EncodeToString(sum[:12])
}

// markContentBoundWithhold tags the withheld action's before_state, so
// parkedWithhold can find it.
func markContentBoundWithhold(f analyzer.Finding, beforeState map[string]any) {
	if contentBoundWithhold(f) {
		beforeState[withheldContentKey] = withheldContent(f)
	}
}

// parkedWithhold reports a finding whose identical content was already
// withheld as unverifiable (lifeos 1.8.3: actions 6385 and 6399 were the
// same refusal, a cycle apart). It is skipped before the gate, so neither
// a decision nor an action row is written again; a new revision with
// other SQL, rollback or target queries lifts the park. A failed read
// lets the finding through, where admission withholds it again.
func (e *Executor) parkedWithhold(ctx context.Context, f analyzer.Finding, findingID int64) bool {
	if e.pool == nil || !contentBoundWithhold(f) {
		return false
	}
	var parked bool
	err := e.pool.QueryRow(ctx, `/* pg_sage */ SELECT EXISTS (
		SELECT 1 FROM sage.action_log
		 WHERE finding_id = $1 AND outcome = 'failed'
		   AND before_state->>'`+withheldContentKey+`' = $2)`,
		findingID, withheldContent(f)).Scan(&parked)
	if err != nil {
		e.logFn("executor", "read withheld record of %q (finding %d): %v",
			f.Title, findingID, err)
		return false
	}
	return parked
}
