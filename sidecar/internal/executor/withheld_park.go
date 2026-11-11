package executor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/analyzer"
)

// withheldContentKey is the before_state key that marks a content-bound
// withhold, and the hash of the content the withhold depends on.
const withheldContentKey = "withheld_content"

// withheldIndexKey names the index whose existing name withheld a build:
// the park lasts only while that name exists (executor-05).
const withheldIndexKey = "withheld_index"

// indexNameTakenError refuses a verified build whose index name exists,
// so the create could be a no-op on another index.
type indexNameTakenError struct{ Index string }

func (e *indexNameTakenError) Error() string {
	return fmt.Sprintf("%s: index %s already exists", ErrVerificationUnavailable, e.Index)
}

func (e *indexNameTakenError) Unwrap() error { return ErrVerificationUnavailable }

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
// parkedWithhold can find it: a content-bound refusal, or a taken index
// name with the index that holds it.
func markContentBoundWithhold(f analyzer.Finding, beforeState map[string]any, refusal error) {
	var taken *indexNameTakenError
	switch {
	case contentBoundWithhold(f):
		beforeState[withheldContentKey] = withheldContent(f)
	case errors.As(refusal, &taken):
		beforeState[withheldContentKey] = withheldContent(f)
		beforeState[withheldIndexKey] = taken.Index
	}
}

// parkedWithhold reports a finding whose identical content was already
// withheld as unverifiable (lifeos 1.8.3: actions 6385 and 6399 were the
// same refusal, a cycle apart). It is skipped before the gate, so neither
// a decision nor an action row is written again; a new revision with
// other SQL, rollback or target queries lifts the park, and so does the
// index whose taken name withheld the build being gone (a failed build's
// remnant cleaned up). A failed read lets the finding through, where
// admission withholds it again.
func (e *Executor) parkedWithhold(ctx context.Context, f analyzer.Finding, findingID int64) bool {
	if e.pool == nil || categorizeAction(f.RecommendedSQL) != "create_index" {
		return false
	}
	var parked bool
	err := e.pool.QueryRow(ctx, `/* pg_sage executor */ SELECT EXISTS (
		SELECT 1 FROM sage.action_log
		 WHERE finding_id = $1 AND outcome = 'failed'
		   AND before_state->>'`+withheldContentKey+`' = $2
		   AND CASE WHEN before_state ? '`+withheldIndexKey+`'
		            THEN to_regclass(before_state->>'`+withheldIndexKey+`') IS NOT NULL
		            ELSE $3 END)`,
		findingID, withheldContent(f), contentBoundWithhold(f)).Scan(&parked)
	if err != nil {
		e.logFn("executor", "read withheld record of %q (finding %d): %v",
			f.Title, findingID, err)
		return false
	}
	return parked
}
