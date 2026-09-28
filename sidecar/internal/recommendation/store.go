package recommendation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store persists recommendations in the sage schema of one database.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore returns a Store over pool.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// withTx runs fn in one transaction.
func (s *Store) withTx(ctx context.Context, fn func(pgx.Tx) error) error {
	if s == nil || s.pool == nil {
		return errors.New("recommendation store has no database")
	}
	return pgx.BeginFunc(ctx, s.pool, fn)
}

// Propose records p: a new head, a new revision when the content hash
// changed, or only a sighting when it did not. A new revision re-proposes
// the head and invalidates any approval of the old content (C04).
func (s *Store) Propose(ctx context.Context, p Proposal) (ProposeResult, error) {
	if err := validateProposal(p); err != nil {
		return ProposeResult{}, err
	}
	var res ProposeResult
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		var err error
		res, err = proposeTx(ctx, tx, p, SourceAnalyzer)
		return err
	})
	if isUniqueViolation(err) {
		// A concurrent writer created the live head first; see it now.
		err = s.withTx(ctx, func(tx pgx.Tx) error {
			var retryErr error
			res, retryErr = proposeTx(ctx, tx, p, SourceAnalyzer)
			return retryErr
		})
	}
	return res, err
}

func validateProposal(p Proposal) error {
	switch {
	case strings.TrimSpace(p.ForwardSQL) == "":
		return errors.New("recommendation proposal has no forward SQL")
	case strings.TrimSpace(p.Category) == "":
		return errors.New("recommendation proposal has no category")
	case strings.TrimSpace(p.Target) == "":
		return errors.New("recommendation proposal has no target")
	}
	return nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// proposeTx is Propose inside tx, with the revision source recorded.
func proposeTx(
	ctx context.Context, tx pgx.Tx, p Proposal, source string,
) (ProposeResult, error) {
	latest, found, err := latestForIdentity(ctx, tx, IdentityKey(p))
	if err != nil {
		return ProposeResult{}, err
	}
	hash := ContentHash(p)
	switch {
	case !found || createAfter(latest, hash):
		return createHead(ctx, tx, p, source)
	case latest.State.Terminal():
		return ProposeResult{Recommendation: latest, Outcome: OutcomeHeld}, nil
	case !CanRevise(latest.State):
		return touch(ctx, tx, latest, p, OutcomeHeld)
	case latest.ContentHash == hash:
		return touch(ctx, tx, latest, p, OutcomeUnchanged)
	default:
		return revise(ctx, tx, latest, p, source)
	}
}

// createAfter reports whether a terminal head lets the identity start a
// new life with content hash. A reverted or abandoned recommendation is
// not re-proposed with the same content: it hurt, or it cannot be done.
func createAfter(latest Recommendation, hash string) bool {
	if !latest.State.Terminal() {
		return false
	}
	refused := latest.State == StateReverted || latest.State == StateAbandoned
	return !refused || latest.ContentHash != hash
}

func latestForIdentity(
	ctx context.Context, tx pgx.Tx, key string,
) (Recommendation, bool, error) {
	rec, err := scanHead(tx.QueryRow(ctx, `/* pg_sage */ SELECT `+headColumns+`
		FROM sage.recommendation r WHERE r.identity_key = $1
		ORDER BY r.id DESC LIMIT 1 FOR UPDATE`, key))
	if errors.Is(err, pgx.ErrNoRows) {
		return Recommendation{}, false, nil
	}
	return rec, err == nil, err
}

// openFindingSQL selects the open finding a proposal belongs to.
const openFindingSQL = `(SELECT id FROM sage.findings
	WHERE category = $1 AND object_identifier = $2 AND status = 'open'
	ORDER BY id DESC LIMIT 1)`

// createHead starts a recommendation for p's open finding. Without one
// there is nothing to act on, so nothing is created.
func createHead(
	ctx context.Context, tx pgx.Tx, p Proposal, source string,
) (ProposeResult, error) {
	var findingID *int64
	if err := tx.QueryRow(ctx, `/* pg_sage */ SELECT `+openFindingSQL,
		p.Category, strings.TrimSpace(p.Target)).Scan(&findingID); err != nil {
		return ProposeResult{}, fmt.Errorf("find open finding: %w", err)
	}
	if findingID == nil {
		return ProposeResult{Outcome: OutcomeNoFinding}, nil
	}
	var id int64
	err := tx.QueryRow(ctx, `/* pg_sage */ INSERT INTO sage.recommendation
		(identity_key, database_name, category, target, action_type,
		 index_fingerprint, finding_id, state, revision, content_hash, retry_budget)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'proposed', 1, $8, $9)
		RETURNING id`,
		IdentityKey(p), p.DatabaseName, p.Category, strings.TrimSpace(p.Target),
		ActionType(p.ForwardSQL), IndexFingerprint(p.ForwardSQL), *findingID,
		ContentHash(p), DefaultRetryBudget).Scan(&id)
	if err != nil {
		return ProposeResult{}, fmt.Errorf("create recommendation: %w", err)
	}
	if err := insertRevision(ctx, tx, id, 1, p, source); err != nil {
		return ProposeResult{}, err
	}
	err = insertTransition(ctx, tx, id, "", StateProposed, 1, ActorAnalyzer, "proposed", nil)
	if err != nil {
		return ProposeResult{}, err
	}
	rec, err := getTx(ctx, tx, id)
	return ProposeResult{Recommendation: rec, Outcome: OutcomeCreated}, err
}

func insertRevision(
	ctx context.Context, tx pgx.Tx, id int64, revision int, p Proposal, source string,
) error {
	evidence := p.Evidence
	if evidence == nil {
		evidence = map[string]any{}
	}
	evidenceJSON, err := json.Marshal(evidence)
	if err != nil {
		return fmt.Errorf("encode recommendation evidence: %w", err)
	}
	pre, err := json.Marshal(preconditions(p))
	if err != nil {
		return fmt.Errorf("encode recommendation preconditions: %w", err)
	}
	_, err = tx.Exec(ctx, `/* pg_sage */ INSERT INTO sage.recommendation_revision
		(recommendation_id, revision, content_hash, forward_sql, inverse_sql, evidence,
		 preconditions, policy_version, title, severity, object_type, action_risk,
		 recommendation, source)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
		id, revision, ContentHash(p), strings.TrimSpace(p.ForwardSQL),
		strings.TrimSpace(p.InverseSQL), evidenceJSON, pre, p.PolicyVersion, p.Title,
		p.Severity, p.ObjectType, p.ActionRisk, p.Recommendation, source)
	if err != nil {
		return fmt.Errorf("insert recommendation revision: %w", err)
	}
	return nil
}

// touch records that the analyzer saw the recommendation again.
func touch(
	ctx context.Context, tx pgx.Tx, rec Recommendation, p Proposal, outcome Outcome,
) (ProposeResult, error) {
	_, err := tx.Exec(ctx, `/* pg_sage */ UPDATE sage.recommendation
		SET last_seen_at = now(),
		    finding_id = COALESCE(`+openFindingSQL+`, finding_id)
		WHERE id = $3`, p.Category, strings.TrimSpace(p.Target), rec.ID)
	if err != nil {
		return ProposeResult{}, fmt.Errorf("touch recommendation %d: %w", rec.ID, err)
	}
	rec, err = getTx(ctx, tx, rec.ID)
	return ProposeResult{Recommendation: rec, Outcome: outcome}, err
}

// revise appends a revision and re-proposes the head. The approval of the
// old content is cleared and queued proposals for it are superseded.
func revise(
	ctx context.Context, tx pgx.Tx, rec Recommendation, p Proposal, source string,
) (ProposeResult, error) {
	next := rec.Revision + 1
	if err := insertRevision(ctx, tx, rec.ID, next, p, source); err != nil {
		return ProposeResult{}, err
	}
	tag, err := tx.Exec(ctx, `/* pg_sage */ UPDATE sage.recommendation
		SET state = 'proposed', revision = $4, content_hash = $5,
		    approved_revision = NULL, approved_hash = NULL, approved_by = NULL,
		    approved_at = NULL, attempt_count = 0, next_attempt_at = NULL,
		    reason = NULL, last_seen_at = now(), updated_at = now(),
		    finding_id = COALESCE(`+openFindingSQL+`, finding_id)
		WHERE id = $3 AND state = $6 AND revision = $7`,
		p.Category, strings.TrimSpace(p.Target), rec.ID, next, ContentHash(p),
		string(rec.State), rec.Revision)
	if err != nil {
		return ProposeResult{}, fmt.Errorf("revise recommendation %d: %w", rec.ID, err)
	}
	if tag.RowsAffected() != 1 {
		return ProposeResult{}, ErrConflict
	}
	reason := fmt.Sprintf("revised: content changed (revision %d)", next)
	err = insertTransition(ctx, tx, rec.ID, rec.State, StateProposed, next, ActorAnalyzer,
		reason, nil)
	if err == nil {
		err = supersedeQueued(ctx, tx, rec.ID, next)
	}
	if err != nil {
		return ProposeResult{}, err
	}
	rec, err = getTx(ctx, tx, rec.ID)
	return ProposeResult{Recommendation: rec, Outcome: OutcomeRevised}, err
}

// supersedeQueued retires queued approvals of older revisions: they can
// never approve the new content.
func supersedeQueued(ctx context.Context, tx pgx.Tx, id int64, revision int) error {
	_, err := tx.Exec(ctx, `/* pg_sage */ UPDATE sage.action_queue
		SET status = 'superseded',
		    reason = 'recommendation revised to revision ' || $2::text
		WHERE recommendation_id = $1 AND recommendation_revision < $2
		  AND status IN ('pending', 'approved', 'failed')`, id, revision)
	if err != nil {
		return fmt.Errorf("supersede queued approvals of recommendation %d: %w", id, err)
	}
	return nil
}

func insertTransition(
	ctx context.Context, tx pgx.Tx, id int64, from, to State, revision int,
	actor, reason string, actionLogID *int64,
) error {
	var fromValue any
	if from != "" {
		fromValue = string(from)
	}
	_, err := tx.Exec(ctx, `/* pg_sage */ INSERT INTO sage.recommendation_transition
		(recommendation_id, from_state, to_state, revision, actor, reason, action_log_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		id, fromValue, string(to), revision, actor, reason, actionLogID)
	if err != nil {
		return fmt.Errorf("record recommendation %d transition: %w", id, err)
	}
	return nil
}
