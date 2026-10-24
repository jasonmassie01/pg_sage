package changereview

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store keeps reviews in the monitored database's sage schema
// (sage.change_review). Every state transition is one conditional UPDATE
// ... RETURNING, so racing writers decide each transition exactly once;
// the loser gets ErrAlreadyDecided. Reads and writes are parameterized
// and tagged.
type Store struct {
	pool     *pgxpool.Pool
	database string
	now      func() time.Time
}

// NewStore is the store in pool's sage schema for database (stored in
// database_name). now defaults to time.Now.
func NewStore(pool *pgxpool.Pool, database string, now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{pool: pool, database: database, now: now}
}

var uuidPattern = regexp.MustCompile(
	`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// reviewColumns is the select list every read scans (scanReview).
const reviewColumns = `id::text, database_name, source, actor, COALESCE(repo, ''),
	COALESCE(pr_url, ''), COALESCE(pr_number, 0), COALESCE(head_sha, ''),
	COALESCE(base_sha, ''), COALESCE(pr_title, ''), pr_opened_at,
	COALESCE(migration_tool, ''), transactional, COALESCE(revision_of::text, ''),
	input_hash, COALESCE(content_hash, ''), COALESCE(diff_sha256, ''), state,
	COALESCE(failure_reason, ''), COALESCE(stage, ''), COALESCE(verdict, ''),
	COALESCE(deterministic_verdict, ''), COALESCE(parse_mode, ''),
	COALESCE(model_status, ''), statements, findings, evidence, dropped, not_determined,
	summary, expectations, %s, COALESCE(markdown, ''), tokens,
	COALESCE(receipt_hash, ''), COALESCE(dba_verdict, ''), COALESCE(dba_direction, ''),
	COALESCE(dba_reason, ''), COALESCE(dba_actor, ''), dba_decided_at,
	COALESCE(verdict_carried_from::text, ''), COALESCE(landing_state, ''),
	COALESCE(landing_source, ''), landed_lower, landed_at, COALESCE(receipt_verdict, ''),
	receipt, receipt_decided_at, shadow_auto_clear, shadow_score, credit_state,
	credited_minutes, toil_model_version, credit_final_at, created_at, completed_at,
	updated_at`

var (
	selectFull  = fmt.Sprintf(reviewColumns, "transcript")
	selectBrief = fmt.Sprintf(reviewColumns, "NULL::jsonb")
)

// reviewRow is a scan target: the review plus its raw jsonb columns.
type reviewRow struct {
	r    Review
	json reviewJSON
}

func (x *reviewRow) dest() []any {
	r, j := &x.r, &x.json
	return []any{&r.ID, &r.Database, &r.Source, &r.Actor, &r.Repo, &r.PRURL, &r.PRNumber,
		&r.HeadSHA, &r.BaseSHA, &r.PRTitle, &r.PROpenedAt, &r.MigrationTool,
		&r.Transactional, &r.RevisionOf, &r.InputHash, &r.ContentHash, &r.DiffSHA256,
		&r.State, &r.FailureReason, &r.Stage, &r.Verdict, &r.DeterministicVerdict,
		&r.ParseMode, &r.ModelStatus, &j.statements, &j.findings, &j.evidence, &j.dropped,
		&j.notDetermined, &j.summary, &j.expectations, &j.transcript, &r.Markdown,
		&r.Tokens, &r.ReceiptHash, &r.DBAVerdict, &r.DBADirection, &r.DBAReason,
		&r.DBAActor, &r.DBADecidedAt, &r.VerdictCarriedFrom, &r.LandingState,
		&r.LandingSource, &r.LandedLower, &r.LandedAt, &r.ReceiptVerdict, &j.receipt,
		&r.ReceiptDecidedAt, &r.ShadowAutoClear, &r.ShadowScore, &r.CreditState,
		&r.CreditedMinutes, &r.ToilModelVersion, &r.CreditFinalAt, &r.CreatedAt,
		&r.CompletedAt, &r.UpdatedAt}
}

// scanReview scans one row of a reviewColumns select.
func scanReview(row pgx.Row) (Review, error) {
	var x reviewRow
	if err := row.Scan(x.dest()...); err != nil {
		return Review{}, err
	}
	if err := x.json.decodeInto(&x.r); err != nil {
		return Review{}, err
	}
	normalizeTimes(&x.r)
	return x.r, nil
}

func scanReviews(rows pgx.Rows) ([]Review, error) {
	defer rows.Close()
	out := []Review{}
	for rows.Next() {
		r, err := scanReview(rows)
		if err != nil {
			return nil, fmt.Errorf("%w: scan review: %v", ErrUnavailable, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: read reviews: %v", ErrUnavailable, err)
	}
	return out, nil
}

// normalizeTimes makes every timestamp UTC, as written.
func normalizeTimes(r *Review) {
	for _, t := range []**time.Time{&r.PROpenedAt, &r.DBADecidedAt, &r.LandedLower,
		&r.LandedAt, &r.ReceiptDecidedAt, &r.CreditFinalAt, &r.CompletedAt} {
		if *t != nil {
			u := (*t).UTC()
			*t = &u
		}
	}
	r.CreatedAt, r.UpdatedAt = r.CreatedAt.UTC(), r.UpdatedAt.UTC()
}

// readErr maps a single-row read error.
func readErr(err error, id string) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: review %s", ErrNotFound, id)
	}
	return fmt.Errorf("%w: read review %s: %v", ErrUnavailable, id, err)
}

// Get is one review with its transcript.
func (st *Store) Get(ctx context.Context, id string) (Review, error) {
	if !uuidPattern.MatchString(id) {
		return Review{}, fmt.Errorf("%w: review %q", ErrNotFound, id)
	}
	r, err := scanReview(st.pool.QueryRow(ctx, `/* pg_sage change_review_get v1 */ SELECT `+
		selectFull+` FROM sage.change_review WHERE id = $1::uuid`, id))
	if err != nil {
		return Review{}, readErr(err, id)
	}
	return r, nil
}

// List is reviews matching f, newest first, without transcripts.
func (st *Store) List(ctx context.Context, f ListFilter) ([]Review, error) {
	if f.Limit == 0 {
		f.Limit = DefaultListLimit
	}
	if f.Limit < 1 || f.Limit > MaxListLimit {
		return nil, invalid("limit must be 1-%d, got %d", MaxListLimit, f.Limit)
	}
	switch f.State {
	case "", StateRunning, StateComplete, StateFailed:
	default:
		return nil, invalid("state %q is not running, complete or failed", f.State)
	}
	if f.PRURL != "" {
		rows, err := st.pool.Query(ctx, `/* pg_sage change_review_list_pr v1 */ SELECT `+
			selectBrief+` FROM sage.change_review WHERE pr_url = $1
			AND ($2 = '' OR state = $2) ORDER BY created_at DESC LIMIT $3`,
			f.PRURL, f.State, f.Limit)
		return listResult(rows, err)
	}
	rows, err := st.pool.Query(ctx, `/* pg_sage change_review_list v1 */ SELECT `+
		selectBrief+` FROM sage.change_review WHERE ($1 = '' OR state = $1)
		ORDER BY created_at DESC LIMIT $2`, f.State, f.Limit)
	return listResult(rows, err)
}

func listResult(rows pgx.Rows, err error) ([]Review, error) {
	if err != nil {
		return nil, fmt.Errorf("%w: list reviews: %v", ErrUnavailable, err)
	}
	return scanReviews(rows)
}

// nullText is s, or NULL when empty.
func nullText(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

// utc is t in UTC at the store's microsecond precision.
func utc(t time.Time) time.Time { return t.UTC().Truncate(time.Microsecond) }

// nullTime is t, or NULL when zero.
func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

// nullInt is n, or NULL when zero.
func nullInt(n int) any {
	if n == 0 {
		return nil
	}
	return n
}
