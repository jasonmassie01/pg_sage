package changereview

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Insert stores r as given (every column) and returns it as stored. An
// empty ID is generated; zero CreatedAt and UpdatedAt are now(). The
// service inserts running reviews; tests and the receipts workstream seed
// reviews in any state.
func (st *Store) Insert(ctx context.Context, r Review) (Review, error) {
	j, err := jsonColumns(r)
	if err != nil {
		return Review{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if r.Database == "" {
		r.Database = st.database
	}
	row := st.pool.QueryRow(ctx, `/* pg_sage change_review_insert v1 */
		INSERT INTO sage.change_review (id, database_name, source, actor, repo, pr_url,
		pr_number, head_sha, base_sha, pr_title, pr_opened_at, migration_tool, transactional,
		revision_of, input_hash, content_hash, diff_sha256, state, failure_reason, stage,
		verdict, deterministic_verdict, parse_mode, model_status, statements, findings,
		evidence, dropped, not_determined, summary, expectations, transcript, markdown,
		tokens, receipt_hash, dba_verdict, dba_direction, dba_reason, dba_actor,
		dba_decided_at, verdict_carried_from, landing_state, landing_source, landed_lower,
		landed_at, receipt_verdict, receipt, receipt_decided_at, shadow_auto_clear,
		shadow_score, credit_state, credited_minutes, toil_model_version, credit_final_at,
		created_at, completed_at, updated_at, landing_checked_at)
		VALUES (COALESCE($1::uuid, gen_random_uuid()), $2, $3, $4, $5, $6, $7, $8, $9, $10,
		$11, $12, $13, $14::uuid, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26,
		$27, $28, $29, $30, $31, $32, $33, $34, $35, $36, $37, $38, $39, $40, $41::uuid, $42,
		$43, $44, $45, $46, $47, $48, $49, COALESCE($50, 'unscored'), COALESCE($51, 'none'),
		$52, $53, $54, COALESCE($55, now()), $56, COALESCE($57, now()), $58)
		RETURNING `+selectFull, append(insertArgs(r, j), unmetAt(r))...)
	out, err := scanReview(row)
	if err != nil {
		return Review{}, fmt.Errorf("%w: store the review: %v", ErrUnavailable, err)
	}
	return out, nil
}

func insertArgs(r Review, j reviewJSON) []any {
	return []any{nullText(r.ID), r.Database, r.Source, r.Actor, nullText(r.Repo),
		nullText(r.PRURL), nullInt(r.PRNumber), nullText(r.HeadSHA), nullText(r.BaseSHA),
		nullText(r.PRTitle), r.PROpenedAt, nullText(r.MigrationTool), r.Transactional,
		nullText(r.RevisionOf), r.InputHash, nullText(r.ContentHash), nullText(r.DiffSHA256),
		r.State, nullText(r.FailureReason), nullText(r.Stage), nullText(r.Verdict),
		nullText(r.DeterministicVerdict), nullText(r.ParseMode), nullText(r.ModelStatus),
		j.statements, j.findings, j.evidence, j.dropped, j.notDetermined, j.summary,
		j.expectations, j.transcript, nullText(r.Markdown), r.Tokens, nullText(r.ReceiptHash),
		nullText(r.DBAVerdict), nullText(r.DBADirection), nullText(r.DBAReason),
		nullText(r.DBAActor), r.DBADecidedAt, nullText(r.VerdictCarriedFrom),
		nullText(r.LandingState), nullText(r.LandingSource), r.LandedLower, r.LandedAt,
		nullText(r.ReceiptVerdict), j.receipt, r.ReceiptDecidedAt, r.ShadowAutoClear,
		nullText(r.ShadowScore), nullText(r.CreditState), r.CreditedMinutes,
		r.ToilModelVersion, r.CreditFinalAt, nullTime(r.CreatedAt), r.CompletedAt,
		nullTime(r.UpdatedAt)}
}

// unmetAt is the landing check a review carries from its own review time:
// its completion, when its post-conditions were unmet then and it waits to
// land (the lower end of a later catalog landing); nil otherwise.
func unmetAt(r Review) *time.Time {
	if r.Expectations.AtReview != AtReviewUnmet || r.LandingState != LandingAwaiting {
		return nil
	}
	return r.CompletedAt
}

// saveDeterministic stores the deterministic result of a running review,
// so it survives a failed model stage, and moves it to the model stage.
func (st *Store) saveDeterministic(ctx context.Context, id string, m Merged,
	parseMode string) error {
	j, err := jsonColumns(Review{Statements: m.Statements, Findings: m.Findings,
		Evidence: m.Evidence})
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	tag, err := st.pool.Exec(ctx, `/* pg_sage change_review_deterministic v1 */
		UPDATE sage.change_review SET stage = $2, deterministic_verdict = $3,
		parse_mode = $4, statements = $5, findings = $6, evidence = $7, updated_at = now()
		WHERE id = $1::uuid AND state = 'running'`, id, StageModel, m.DeterministicVerdict,
		parseMode, j.statements, j.findings, j.evidence)
	if err != nil {
		return fmt.Errorf("%w: store the deterministic result: %v", ErrUnavailable, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: review %s is no longer running", ErrAlreadyDecided, id)
	}
	return nil
}

// setStage records the stage of a running review.
func (st *Store) setStage(ctx context.Context, id, stage string) error {
	_, err := st.pool.Exec(ctx, `/* pg_sage change_review_stage v1 */
		UPDATE sage.change_review SET stage = $2, updated_at = now()
		WHERE id = $1::uuid AND state = 'running'`, id, stage)
	if err != nil {
		return fmt.Errorf("%w: record the stage: %v", ErrUnavailable, err)
	}
	return nil
}

// complete stores the finished review r (state complete) once, from
// running.
func (st *Store) complete(ctx context.Context, r Review) (Review, error) {
	j, err := jsonColumns(r)
	if err != nil {
		return Review{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	row := st.pool.QueryRow(ctx, `/* pg_sage change_review_complete v1 */
		UPDATE sage.change_review SET state = 'complete', stage = $2, content_hash = $3,
		verdict = $4, deterministic_verdict = $5, parse_mode = $6, model_status = $7,
		statements = $8, findings = $9, evidence = $10, dropped = $11, not_determined = $12,
		summary = $13, expectations = $14, transcript = $15, markdown = $16, tokens = $17,
		dba_verdict = $18, dba_direction = $19, dba_reason = $20, dba_actor = $21,
		dba_decided_at = $22, verdict_carried_from = $23::uuid, landing_state = $27,
		receipt_verdict = 'pending', shadow_auto_clear = $24, credit_state = $25,
		completed_at = $26, landing_checked_at = $28, updated_at = now()
		WHERE id = $1::uuid AND state = 'running' RETURNING `+selectFull,
		r.ID, StageDone, nullText(r.ContentHash), r.Verdict, r.DeterministicVerdict,
		r.ParseMode, r.ModelStatus, j.statements, j.findings, j.evidence, j.dropped,
		j.notDetermined, j.summary, j.expectations, j.transcript, r.Markdown, r.Tokens,
		nullText(r.DBAVerdict), nullText(r.DBADirection), nullText(r.DBAReason),
		nullText(r.DBAActor), r.DBADecidedAt, nullText(r.VerdictCarriedFrom),
		r.ShadowAutoClear, r.CreditState, r.CompletedAt, r.LandingState, unmetAt(r))
	out, err := scanReview(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Review{}, fmt.Errorf("%w: review %s is no longer running", ErrAlreadyDecided,
			r.ID)
	}
	if err != nil {
		return Review{}, fmt.Errorf("%w: complete review %s: %v", ErrUnavailable, r.ID, err)
	}
	return out, nil
}

// fail marks a running review failed with reason, once.
func (st *Store) fail(ctx context.Context, id, reason string) (Review, error) {
	row := st.pool.QueryRow(ctx, `/* pg_sage change_review_fail v1 */
		UPDATE sage.change_review SET state = 'failed', failure_reason = $2,
		completed_at = now(), updated_at = now()
		WHERE id = $1::uuid AND state = 'running' RETURNING `+selectFull, id,
		clipRunes(reason, MaxReasonRunes))
	out, err := scanReview(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Review{}, fmt.Errorf("%w: review %s is no longer running", ErrAlreadyDecided, id)
	}
	if err != nil {
		return Review{}, fmt.Errorf("%w: fail review %s: %v", ErrUnavailable, id, err)
	}
	return out, nil
}

// InterruptStale fails reviews left running, untouched for longer than
// olderThan, with reason interrupted; it returns how many.
func (st *Store) InterruptStale(ctx context.Context, olderThan time.Duration) (int, error) {
	tag, err := st.pool.Exec(ctx, `/* pg_sage change_review_interrupt v1 */
		UPDATE sage.change_review SET state = 'failed', failure_reason = $1,
		completed_at = now(), updated_at = now()
		WHERE state = 'running' AND updated_at < now() - make_interval(secs => $2)`,
		FailureInterrupted, olderThan.Seconds())
	if err != nil {
		return 0, fmt.Errorf("%w: interrupt stale reviews: %v", ErrUnavailable, err)
	}
	return int(tag.RowsAffected()), nil
}
