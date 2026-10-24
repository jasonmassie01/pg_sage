package changereview

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Budget is Change Review's persisted daily token budget for one database
// (sage.change_review_budget_day), the internal/ask budget pattern: every
// model call is reserved against the database total (actor '*') and the
// caller's own allocation before it is sent, in one transaction with both
// rows locked, and settled with the usage the provider reported. The LLM
// client's own llm.token_budget_daily still applies on top.
type Budget struct {
	pool        *pgxpool.Pool
	perDatabase int64
	perActor    int64
	now         func() time.Time
}

// Budget scopes.
const (
	BudgetScopeDatabase = "database"
	BudgetScopeActor    = "actor"
	budgetDatabaseActor = "*"
)

// BudgetError is a refused reservation: which allocation, what is used,
// its limit and what the call needed. It wraps ErrBudgetExhausted.
type BudgetError struct {
	Scope string
	Used  int64
	Limit int64
	Need  int64
}

func (e *BudgetError) Error() string {
	return fmt.Sprintf("%v: the %s allocation has %d of %d tokens used; the call needs %d",
		ErrBudgetExhausted, e.Scope, e.Used, e.Limit, e.Need)
}

func (e *BudgetError) Unwrap() error { return ErrBudgetExhausted }

// BudgetStatus is today's usage of the database and one actor.
type BudgetStatus struct {
	Day           string `json:"day"`
	DatabaseUsed  int64  `json:"database_used"`
	DatabaseLimit int64  `json:"database_limit"`
	ActorUsed     int64  `json:"actor_used"`
	ActorLimit    int64  `json:"actor_limit"`
}

// NewBudget is the budget in pool's sage schema; now defaults to time.Now.
func NewBudget(pool *pgxpool.Pool, perDatabase, perActor int64,
	now func() time.Time) *Budget {
	if now == nil {
		now = time.Now
	}
	return &Budget{pool: pool, perDatabase: perDatabase, perActor: perActor, now: now}
}

// day is the UTC day of the budget's clock.
func (b *Budget) day() time.Time {
	t := b.now().UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// Reserve charges tokens to the database and the actor, or refuses with
// a *BudgetError when either allocation cannot pay for it.
func (b *Budget) Reserve(ctx context.Context, actor string, tokens int64) error {
	if err := checkActor(actor); err != nil {
		return err
	}
	if tokens <= 0 {
		return invalid("reservation of %d tokens", tokens)
	}
	day := b.day()
	err := pgx.BeginFunc(ctx, b.pool, func(tx pgx.Tx) error {
		dbUsed, actorUsed, err := lockBudgetRows(ctx, tx, day, actor)
		if err != nil {
			return err
		}
		switch {
		case dbUsed+tokens > b.perDatabase:
			return &BudgetError{Scope: BudgetScopeDatabase, Used: dbUsed,
				Limit: b.perDatabase, Need: tokens}
		case actorUsed+tokens > b.perActor:
			return &BudgetError{Scope: BudgetScopeActor, Used: actorUsed,
				Limit: b.perActor, Need: tokens}
		}
		return addBudget(ctx, tx, day, actor, tokens)
	})
	return budgetErr(err, "reserve")
}

// lockBudgetRows creates the day's rows if needed and locks them, the
// database row first, so concurrent reservations serialize without
// deadlock.
func lockBudgetRows(ctx context.Context, tx pgx.Tx, day time.Time, actor string) (int64,
	int64, error) {
	if _, err := tx.Exec(ctx, `/* pg_sage change_review_budget_rows v1 */
		INSERT INTO sage.change_review_budget_day (day, actor) VALUES ($1, $2), ($1, $3)
		ON CONFLICT (day, actor) DO NOTHING`, day, budgetDatabaseActor, actor); err != nil {
		return 0, 0, err
	}
	rows, err := tx.Query(ctx, `/* pg_sage change_review_budget_lock v1 */
		SELECT actor, tokens FROM sage.change_review_budget_day
		WHERE day = $1 AND actor IN ($2, $3) ORDER BY actor = $2 DESC FOR UPDATE`, day,
		budgetDatabaseActor, actor)
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()
	var dbUsed, actorUsed int64
	for rows.Next() {
		var who string
		var n int64
		if err := rows.Scan(&who, &n); err != nil {
			return 0, 0, err
		}
		if who == budgetDatabaseActor {
			dbUsed = n
		} else {
			actorUsed = n
		}
	}
	return dbUsed, actorUsed, rows.Err()
}

// addBudget applies delta to both rows, never below zero.
func addBudget(ctx context.Context, tx pgx.Tx, day time.Time, actor string,
	delta int64) error {
	_, err := tx.Exec(ctx, `/* pg_sage change_review_budget_add v1 */
		UPDATE sage.change_review_budget_day
		SET tokens = GREATEST(0, tokens + $4), updated_at = now()
		WHERE day = $1 AND actor IN ($2, $3)`, day, budgetDatabaseActor, actor, delta)
	return err
}

// Settle trues a reservation up (or down) to the reported usage.
func (b *Budget) Settle(ctx context.Context, actor string, reserved, used int64) error {
	if err := checkActor(actor); err != nil {
		return err
	}
	if used < 0 || reserved < 0 {
		return invalid("settle %d of %d reserved tokens", used, reserved)
	}
	if used == reserved {
		return nil
	}
	day := b.day()
	err := pgx.BeginFunc(ctx, b.pool, func(tx pgx.Tx) error {
		if _, _, err := lockBudgetRows(ctx, tx, day, actor); err != nil {
			return err
		}
		return addBudget(ctx, tx, day, actor, used-reserved)
	})
	return budgetErr(err, "settle")
}

// Status is today's usage of the database and actor.
func (b *Budget) Status(ctx context.Context, actor string) (BudgetStatus, error) {
	if err := checkActor(actor); err != nil {
		return BudgetStatus{}, err
	}
	day := b.day()
	st := BudgetStatus{Day: day.Format("2006-01-02"), DatabaseLimit: b.perDatabase,
		ActorLimit: b.perActor}
	err := b.pool.QueryRow(ctx, `/* pg_sage change_review_budget_status v1 */ SELECT
		COALESCE(max(tokens) FILTER (WHERE actor = $2), 0),
		COALESCE(max(tokens) FILTER (WHERE actor = $3), 0)
		FROM sage.change_review_budget_day WHERE day = $1 AND actor IN ($2, $3)`, day,
		budgetDatabaseActor, actor).Scan(&st.DatabaseUsed, &st.ActorUsed)
	return st, budgetErr(err, "read")
}

// budgetErr keeps a refusal as is and marks store failures unavailable.
func budgetErr(err error, op string) error {
	var be *BudgetError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &be):
		return be
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	}
	return fmt.Errorf("%w: %s the Change Review budget: %v", ErrUnavailable, op, err)
}
