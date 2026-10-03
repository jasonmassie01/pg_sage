package ledger

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// The standing gate evaluates every candidate every cycle. An unchanged
// withheld verdict (parked, queued, blocked, observe-only) is one fact, not
// one fact per cycle (dogfood lifeos: ~41,000 rows an hour): repeats of an
// open fingerprint update one row, counting them in repeat_count and
// stamping last_seen_at, with the latest reason and evidence. Execute
// verdicts have no fingerprint: each backs its own action and keeps its
// own row.

// FingerprintRepository is a Repository that can fold repeats of an open
// fingerprinted decision into one row. It returns the row's id and its
// (first) evidence ID.
type FingerprintRepository interface {
	UpsertDecision(context.Context, DecisionInput) (int64, string, error)
}

// DecisionFingerprint identifies a repeat of the same decision: database,
// feature, intent, targets (in any order), verdict, policy version and the
// proposed SQL. The reason and evidence are not part of it; a repeat
// records the latest ones.
func DecisionFingerprint(input DecisionInput) string {
	database := ""
	if input.DatabaseID != nil {
		database = strconv.Itoa(*input.DatabaseID)
	}
	targets := append([]string(nil), input.TargetObjects...)
	sort.Strings(targets)
	sum := sha256.Sum256([]byte(strings.Join([]string{
		database, input.Feature, input.Intent, strings.Join(targets, "\x1e"),
		string(input.Verdict), strconv.Itoa(input.PolicyVersion), input.ProposedSQL,
	}, "\x1f")))
	return "fp_" + hex.EncodeToString(sum[:16])
}

// missingConflictIndex reports PostgreSQL's 42P10: ON CONFLICT names no
// usable unique index (idx_decision_fingerprint dropped or INVALID).
func missingConflictIndex(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "42P10"
}

// UpsertDecision inserts a fingerprinted decision or, when an open row has
// the fingerprint, counts the repeat on it. Without the unique index it
// falls back to a plain insert: every repeat is then its own row, as
// before, and an insert failure is returned.
func (r *PostgresRepository) UpsertDecision(
	ctx context.Context, input DecisionInput,
) (int64, string, error) {
	values, err := decisionValues(input)
	if err != nil {
		return 0, "", err
	}
	values[len(values)-1] = input.Fingerprint
	var id int64
	var evidenceID string
	err = r.pool.QueryRow(ctx, upsertDecisionSQL, values...).Scan(&id, &evidenceID)
	if missingConflictIndex(err) {
		id, err = r.InsertDecision(ctx, input)
		return id, input.EvidenceID, err
	}
	if err != nil {
		return 0, "", fmt.Errorf("persist fingerprinted decision: %w", err)
	}
	return id, evidenceID, nil
}

// upsertDecisionSQL bumps only unindexed columns on a repeat, so the
// update stays HOT unless the deadline changed.
const upsertDecisionSQL = `INSERT INTO sage.decision
	(database_id, feature, intent, target_objects, policy_version, verdict,
	 risk_tier, reason, evidence, evidence_id, deadline_kind, deadline_hard_at,
	 fingerprint)
	VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,NULLIF($11,''),$12,$13)
	ON CONFLICT (fingerprint) WHERE fingerprint IS NOT NULL AND resolved_at IS NULL
	DO UPDATE SET repeat_count = sage.decision.repeat_count + 1,
	              last_seen_at = now(), reason = EXCLUDED.reason,
	              risk_tier = EXCLUDED.risk_tier, evidence = EXCLUDED.evidence,
	              deadline_kind = EXCLUDED.deadline_kind,
	              deadline_hard_at = EXCLUDED.deadline_hard_at
	RETURNING id, evidence_id`
