package facts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// confirmedTTL bounds how stale the gate's view of confirmed facts may be
// across processes; writes through any Store on the same pool in this
// process invalidate it at once.
const confirmedTTL = 10 * time.Second

// Store keeps the facts of one monitored database (sage.facts).
type Store struct {
	pool   *pgxpool.Pool
	shared *sharedState
	grace  time.Duration
	now    func() time.Time
	logFn  func(string, string, ...any)
}

// sharedState is the per-pool cache of confirmed facts and the absences
// re-verification has seen, shared by every Store on the pool.
type sharedState struct {
	mu       sync.Mutex
	cached   []Fact
	cachedAt time.Time
	valid    bool
	missing  map[int64]time.Time
}

var sharedByPool sync.Map // *pgxpool.Pool -> *sharedState

// NewStore returns the fact store of the database behind pool.
func NewStore(pool *pgxpool.Pool) *Store {
	state, _ := sharedByPool.LoadOrStore(pool, &sharedState{missing: map[int64]time.Time{}})
	return &Store{pool: pool, shared: state.(*sharedState), grace: time.Hour,
		now: time.Now, logFn: func(string, string, ...any) {}}
}

// WithMissingGrace sets how long a fact's objects may be absent before
// re-verification expires it.
func (s *Store) WithMissingGrace(d time.Duration) *Store {
	c := *s
	c.grace = d
	return &c
}

// WithLog sets the store's logger.
func (s *Store) WithLog(logFn func(string, string, ...any)) *Store {
	c := *s
	if logFn != nil {
		c.logFn = logFn
	}
	return &c
}

// ownTypes keeps this store to its own fact types: column classes
// (fact_type column_class) share sage.facts but belong to agent
// governance's classify package, have their own API and never bind here.
const ownTypes = `fact_type <> 'column_class'`

const factColumns = `id, fact_type, subject_kind, subject, value, source, proposed_by,
	evidence, rationale, status, COALESCE(decided_by, ''), decided_at, decision_note,
	expires_at, expired_reason, proposals, created_at, updated_at, last_verified_at`

func scanFact(row pgx.Row, extra ...any) (Fact, error) {
	var f Fact
	var value, evidence []byte
	dest := []any{&f.ID, &f.Type, &f.Kind, &f.Subject, &value, &f.Source, &f.ProposedBy,
		&evidence, &f.Rationale, &f.Status, &f.DecidedBy, &f.DecidedAt, &f.DecisionNote,
		&f.ExpiresAt, &f.ExpiredReason, &f.Proposals, &f.CreatedAt, &f.UpdatedAt,
		&f.LastVerifiedAt}
	if err := row.Scan(append(dest, extra...)...); err != nil {
		return Fact{}, err
	}
	if err := json.Unmarshal(value, &f.Value); err != nil {
		return Fact{}, fmt.Errorf("decode fact %d value: %w", f.ID, err)
	}
	if err := json.Unmarshal(evidence, &f.Evidence); err != nil {
		return Fact{}, fmt.Errorf("decode fact %d evidence: %w", f.ID, err)
	}
	if f.Value == nil {
		f.Value = map[string]string{}
	}
	return f, nil
}

const proposeSQL = `/* pg_sage */
WITH prior AS (SELECT status FROM sage.facts
               WHERE fact_type = $1 AND subject_kind = $2 AND subject = $3)
INSERT INTO sage.facts AS f (fact_type, subject_kind, subject, value, source, proposed_by,
                             evidence, rationale, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT (fact_type, subject_kind, subject) DO UPDATE SET
    proposals = f.proposals + 1,
    evidence = (SELECT COALESCE(jsonb_agg(x.e ORDER BY x.o), '[]'::jsonb)
                FROM jsonb_array_elements(EXCLUDED.evidence || f.evidence)
                     WITH ORDINALITY AS x(e, o) WHERE x.o <= 10),
    value = CASE WHEN f.status IN ('proposed', 'expired') THEN EXCLUDED.value
                 ELSE f.value END,
    rationale = CASE WHEN f.status IN ('proposed', 'expired') AND EXCLUDED.rationale <> ''
                     THEN EXCLUDED.rationale ELSE f.rationale END,
    source = CASE WHEN f.status = 'expired' THEN EXCLUDED.source ELSE f.source END,
    proposed_by = CASE WHEN f.status = 'expired' THEN EXCLUDED.proposed_by
                       ELSE f.proposed_by END,
    expires_at = CASE WHEN f.status = 'expired' THEN EXCLUDED.expires_at
                      ELSE f.expires_at END,
    decided_by = CASE WHEN f.status = 'expired' THEN NULL ELSE f.decided_by END,
    decided_at = CASE WHEN f.status = 'expired' THEN NULL ELSE f.decided_at END,
    decision_note = CASE WHEN f.status = 'expired' THEN '' ELSE f.decision_note END,
    expired_reason = CASE WHEN f.status = 'expired' THEN '' ELSE f.expired_reason END,
    status = CASE WHEN f.status = 'expired' THEN 'proposed' ELSE f.status END,
    updated_at = now()
RETURNING ` + factColumns + `, (xmax = 0), COALESCE((SELECT status FROM prior), '')`

// Propose records a proposal. A proposal of an existing fact counts once
// more and adds its evidence; it never changes a confirmed or rejected
// fact's decision, and it reopens an expired fact (new evidence). created
// reports a new or reopened fact, the one an operator must now decide.
func (s *Store) Propose(ctx context.Context, p Proposal) (Fact, bool, error) {
	p, err := Validate(p)
	if err != nil {
		return Fact{}, false, err
	}
	value, evidence, err := encodeProposal(p)
	if err != nil {
		return Fact{}, false, err
	}
	var inserted bool
	var prior string
	f, err := scanFact(s.pool.QueryRow(ctx, proposeSQL, p.Type, p.Kind, p.Subject, value,
		p.Source, p.ProposedBy, evidence, p.Rationale, p.ExpiresAt), &inserted, &prior)
	if err != nil {
		return Fact{}, false, fmt.Errorf("record fact proposal: %w", err)
	}
	return f, inserted || prior == string(StatusExpired), nil
}

func encodeProposal(p Proposal) ([]byte, []byte, error) {
	value, err := json.Marshal(p.Value)
	if err != nil {
		return nil, nil, fmt.Errorf("encode fact value: %w", err)
	}
	if p.Evidence == nil {
		p.Evidence = []Citation{}
	}
	evidence, err := json.Marshal(p.Evidence)
	if err != nil {
		return nil, nil, fmt.Errorf("encode fact evidence: %w", err)
	}
	return value, evidence, nil
}

const declareSQL = `/* pg_sage */
INSERT INTO sage.facts AS f (fact_type, subject_kind, subject, value, source, proposed_by,
                             evidence, rationale, expires_at, status, decided_by,
                             decided_at, decision_note)
VALUES ($1, $2, $3, $4, 'operator', $5, $6, $7, $8, 'confirmed', $5, now(), $9)
ON CONFLICT (fact_type, subject_kind, subject) DO UPDATE SET
    value = EXCLUDED.value, source = 'operator', proposed_by = EXCLUDED.proposed_by,
    rationale = EXCLUDED.rationale, expires_at = EXCLUDED.expires_at,
    status = 'confirmed', decided_by = EXCLUDED.decided_by, decided_at = now(),
    decision_note = EXCLUDED.decision_note, expired_reason = '',
    proposals = f.proposals + 1, updated_at = now()
RETURNING ` + factColumns

// Declare records an operator's own fact, confirmed by the declaration
// (over any earlier proposal or rejection of the same fact).
func (s *Store) Declare(ctx context.Context, p Proposal, actor, note string) (Fact, error) {
	actor, err := validActor(actor)
	if err != nil {
		return Fact{}, err
	}
	p.Source, p.ProposedBy = SourceOperator, actor
	if p, err = Validate(p); err != nil {
		return Fact{}, err
	}
	value, evidence, err := encodeProposal(p)
	if err != nil {
		return Fact{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Fact{}, fmt.Errorf("declare fact: %w", err)
	}
	defer rollbackQuietly(ctx, tx)
	f, err := scanFact(tx.QueryRow(ctx, declareSQL, p.Type, p.Kind, p.Subject, value,
		actor, evidence, p.Rationale, p.ExpiresAt, clip(cleanText(note), 1000)))
	if err != nil {
		return Fact{}, fmt.Errorf("declare fact: %w", err)
	}
	if err := registerSlot(ctx, tx, f); err != nil {
		return Fact{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Fact{}, fmt.Errorf("declare fact: %w", err)
	}
	s.invalidate()
	return f, nil
}

func validActor(actor string) (string, error) {
	actor = strings.TrimSpace(cleanText(actor))
	if actor == "" || len(actor) > maxProposedBy {
		return "", fmt.Errorf("%w: a decision names who made it (1-%d characters)",
			ErrInvalidValue, maxProposedBy)
	}
	return actor, nil
}

func rollbackQuietly(ctx context.Context, tx pgx.Tx) {
	// Rollback after Commit is a no-op (pgx.ErrTxClosed); any other error
	// leaves the connection to pgx, which discards a broken one.
	_ = tx.Rollback(ctx)
}

// Get returns one fact.
func (s *Store) Get(ctx context.Context, id int64) (Fact, error) {
	f, err := scanFact(s.pool.QueryRow(ctx, `/* pg_sage */ SELECT `+factColumns+`
		FROM sage.facts WHERE id = $1 AND `+ownTypes, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Fact{}, ErrNotFound
	}
	if err != nil {
		return Fact{}, fmt.Errorf("read fact %d: %w", id, err)
	}
	return f, nil
}

// List returns facts matching filter, newest first (at most 1000).
func (s *Store) List(ctx context.Context, filter Filter) ([]Fact, error) {
	statuses := make([]string, 0, len(filter.Status))
	for _, st := range filter.Status {
		switch st {
		case StatusProposed, StatusConfirmed, StatusRejected, StatusExpired:
			statuses = append(statuses, string(st))
		default:
			return nil, fmt.Errorf("%w: status %q", ErrInvalidValue, st)
		}
	}
	if _, ok := typeKinds[filter.Type]; filter.Type != "" && !ok {
		return nil, fmt.Errorf("%w: type %q", ErrInvalidValue, filter.Type)
	}
	limit := filter.Limit
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	rows, err := s.pool.Query(ctx, `/* pg_sage */ SELECT `+factColumns+`
		FROM sage.facts WHERE `+ownTypes+`
		  AND (cardinality($1::text[]) = 0 OR status = ANY($1))
		  AND ($2 = '' OR fact_type = $2) ORDER BY id DESC LIMIT $3`,
		statuses, string(filter.Type), limit)
	if err != nil {
		return nil, fmt.Errorf("list facts: %w", err)
	}
	return collectFacts(rows)
}

func collectFacts(rows pgx.Rows) ([]Fact, error) {
	defer rows.Close()
	out := []Fact{}
	for rows.Next() {
		f, err := scanFact(rows)
		if err != nil {
			return nil, fmt.Errorf("scan fact: %w", err)
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read facts: %w", err)
	}
	return out, nil
}

// Confirmed returns the confirmed, unexpired facts in id order, cached for
// at most confirmedTTL.
func (s *Store) Confirmed(ctx context.Context) ([]Fact, error) {
	st := s.shared
	st.mu.Lock()
	if st.valid && s.now().Sub(st.cachedAt) < confirmedTTL {
		out := append([]Fact(nil), st.cached...)
		st.mu.Unlock()
		return out, nil
	}
	st.mu.Unlock()
	rows, err := s.pool.Query(ctx, `/* pg_sage */ SELECT `+factColumns+`
		FROM sage.facts WHERE `+ownTypes+` AND status = 'confirmed'
		  AND (expires_at IS NULL OR expires_at > now()) ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("read confirmed facts: %w", err)
	}
	facts, err := collectFacts(rows)
	if err != nil {
		return nil, err
	}
	st.mu.Lock()
	st.cached, st.cachedAt, st.valid = facts, s.now(), true
	st.mu.Unlock()
	return append([]Fact(nil), facts...), nil
}

// invalidate drops the cached confirmed facts.
func (s *Store) invalidate() {
	s.shared.mu.Lock()
	s.shared.valid = false
	s.shared.mu.Unlock()
}
