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

	"github.com/jackc/pgx/v5"
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
	parts := []string{
		database, input.Feature, input.Intent, strings.Join(targets, "\x1e"),
		string(input.Verdict), strconv.Itoa(input.PolicyVersion), input.ProposedSQL,
	}
	if input.PrincipalID != "" { // pg_sage's own fingerprints are unchanged
		parts = append(parts, "principal="+input.PrincipalID)
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x1f")))
	return "fp_" + hex.EncodeToString(sum[:16])
}

// CandidateKey identifies the candidate a decision is about, whatever the
// gate decided: database, feature, targets (in any order), the proposed SQL
// and the principal. Verdict, intent and policy version are not part of
// it, so an operator's execute and an autonomous park of the same change
// share it. An execute ends the candidate's open withheld rows (an
// episode); the next withheld verdict starts a new row.
func CandidateKey(input DecisionInput) string {
	database := ""
	if input.DatabaseID != nil {
		database = strconv.Itoa(*input.DatabaseID)
	}
	targets := append([]string(nil), input.TargetObjects...)
	sort.Strings(targets)
	parts := []string{database, input.Feature, strings.Join(targets, "\x1e"),
		input.ProposedSQL, input.PrincipalID}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x1f")))
	return "ck_" + hex.EncodeToString(sum[:16])
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
	values[fingerprintParam] = input.Fingerprint
	var id int64
	var evidenceID string
	err = r.pool.QueryRow(ctx, upsertDecisionSQL, values...).Scan(&id, &evidenceID)
	if missingConflictIndex(err) {
		id, err = r.InsertDecision(ctx, input)
		return id, input.EvidenceID, err
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return r.retierDecision(ctx, input, values)
	}
	if err != nil {
		return 0, "", fmt.Errorf("persist fingerprinted decision: %w", err)
	}
	return id, evidenceID, nil
}

// retierDecision records a repeat whose risk tier differs from the open
// row's (the upsert updated nothing). A decision's tier is its authority,
// sealed in the audit chain (audit-11), so the open row is never re-tiered:
// it is resolved and the repeat starts a new decision, in one transaction,
// so the fingerprint keeps exactly one open row. A writer racing it at yet
// another tier is refused, never merged: the gate's next cycle records it.
func (r *PostgresRepository) retierDecision(
	ctx context.Context, input DecisionInput, values []any,
) (int64, string, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, "", fmt.Errorf("re-tier decision %s: begin: %w", input.Fingerprint, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, retireDecisionSQL, input.Fingerprint,
		input.RiskTier); err != nil {
		return 0, "", fmt.Errorf("re-tier decision %s: resolve the open row at another "+
			"tier: %w", input.Fingerprint, err)
	}
	var id int64
	var evidenceID string
	err = tx.QueryRow(ctx, upsertDecisionSQL, values...).Scan(&id, &evidenceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, "", fmt.Errorf("%w: decision %s was re-tiered concurrently; the next "+
			"evaluation records it", ErrDecisionRetiered, input.Fingerprint)
	}
	if err != nil {
		return 0, "", fmt.Errorf("re-tier decision %s at %s: %w", input.Fingerprint,
			input.RiskTier, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, "", fmt.Errorf("re-tier decision %s: commit: %w", input.Fingerprint, err)
	}
	return id, evidenceID, nil
}

// ErrDecisionRetiered reports a fingerprint repeated at two tiers at once.
var ErrDecisionRetiered = errors.New("decision re-tiered concurrently")

// retireDecisionSQL resolves the fingerprint's open row when its tier is
// not the repeat's.
const retireDecisionSQL = `/* pg_sage */ UPDATE sage.decision SET resolved_at = now()
	WHERE fingerprint = $1 AND resolved_at IS NULL AND risk_tier IS DISTINCT FROM $2`

// upsertDecisionSQL bumps only unindexed columns on a repeat, so the
// update stays HOT unless the deadline changed. candidate_key is indexed
// but unchanged on a repeat (it is derived from the fingerprint's inputs),
// except on a row written before policy-02, which gains its key here. A
// repeat records the latest guardrails and deadline override; the policy
// hash is the version's (the version is in the fingerprint), so it stays.
// A repeat at another risk tier updates nothing and returns no row
// (retierDecision): the tier of a recorded decision never changes.
const upsertDecisionSQL = insertDecisionColumns + `
	ON CONFLICT (fingerprint) WHERE fingerprint IS NOT NULL AND resolved_at IS NULL
	DO UPDATE SET repeat_count = sage.decision.repeat_count + 1,
	              last_seen_at = now(), reason = EXCLUDED.reason,
	              evidence = EXCLUDED.evidence,
	              deadline_kind = EXCLUDED.deadline_kind,
	              deadline_hard_at = EXCLUDED.deadline_hard_at,
	              candidate_key = EXCLUDED.candidate_key,
	              guardrails = EXCLUDED.guardrails,
	              off_window_ok = EXCLUDED.off_window_ok
	WHERE sage.decision.risk_tier = EXCLUDED.risk_tier
	RETURNING id, evidence_id`

// fingerprintParam is the index of $13, fingerprint, in decisionValues.
const fingerprintParam = 12
