package ask

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Budget is Ask Sage's persisted daily token budget for one database
// (sage.ask_budget_day): every model call is reserved against the
// database's total (actor '*') and the caller's own allocation before it
// is sent, in one transaction with both rows locked, and settled with the
// usage the provider reported. It is separate from the LLM client's
// daily budget, which the investigator and the tuning agent share.
type Budget struct {
	pool          *pgxpool.Pool
	perDatabase   int64
	perUser       int64
	now           func() time.Time
	databaseActor string
}

// Budget scopes.
const (
	BudgetScopeDatabase = "database"
	BudgetScopeUser     = "user"
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
	UserUsed      int64  `json:"user_used"`
	UserLimit     int64  `json:"user_limit"`
}

// NewBudget is the budget in pool's sage schema.
func NewBudget(pool *pgxpool.Pool, perDatabase, perUser int64,
	now func() time.Time) *Budget {
	if now == nil {
		now = time.Now
	}
	return &Budget{pool: pool, perDatabase: perDatabase, perUser: perUser, now: now,
		databaseActor: "*"}
}

func (b *Budget) day() time.Time {
	t := b.now().UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func checkActor(actor string) error {
	a := strings.TrimSpace(actor)
	if a == "" || a == "*" || utf8.RuneCountInString(actor) > 200 {
		return fmt.Errorf("%w: invalid budget actor %q", ErrInvalid, actor)
	}
	return nil
}

// Reserve charges tokens to the database and the actor, or refuses with
// a *BudgetError when either allocation cannot pay for it.
func (b *Budget) Reserve(ctx context.Context, actor string, tokens int64) error {
	if err := checkActor(actor); err != nil {
		return err
	}
	if tokens <= 0 {
		return fmt.Errorf("%w: reservation of %d tokens", ErrInvalid, tokens)
	}
	err := pgx.BeginFunc(ctx, b.pool, func(tx pgx.Tx) error {
		dbUsed, userUsed, err := b.lockRows(ctx, tx, actor)
		if err != nil {
			return err
		}
		switch {
		case dbUsed+tokens > b.perDatabase:
			return &BudgetError{Scope: BudgetScopeDatabase, Used: dbUsed,
				Limit: b.perDatabase, Need: tokens}
		case userUsed+tokens > b.perUser:
			return &BudgetError{Scope: BudgetScopeUser, Used: userUsed, Limit: b.perUser,
				Need: tokens}
		}
		return b.add(ctx, tx, actor, tokens)
	})
	return b.wrap(err, "reserve")
}

// lockRows creates today's rows if needed and locks them, the database
// row first, so concurrent reservations serialize without deadlock.
func (b *Budget) lockRows(ctx context.Context, tx pgx.Tx, actor string) (int64, int64,
	error) {
	if _, err := tx.Exec(ctx, `/* pg_sage */ INSERT INTO sage.ask_budget_day (day, actor)
		VALUES ($1, $2), ($1, $3) ON CONFLICT (day, actor) DO NOTHING`, b.day(),
		b.databaseActor, actor); err != nil {
		return 0, 0, err
	}
	rows, err := tx.Query(ctx, `/* pg_sage */ SELECT actor, tokens FROM sage.ask_budget_day
		WHERE day = $1 AND actor IN ($2, $3) ORDER BY actor = $2 DESC FOR UPDATE`, b.day(),
		b.databaseActor, actor)
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()
	var dbUsed, userUsed int64
	for rows.Next() {
		var who string
		var n int64
		if err := rows.Scan(&who, &n); err != nil {
			return 0, 0, err
		}
		if who == b.databaseActor {
			dbUsed = n
		} else {
			userUsed = n
		}
	}
	return dbUsed, userUsed, rows.Err()
}

// add applies delta to both rows, never below zero.
func (b *Budget) add(ctx context.Context, tx pgx.Tx, actor string, delta int64) error {
	_, err := tx.Exec(ctx, `/* pg_sage */ UPDATE sage.ask_budget_day
		SET tokens = GREATEST(0, tokens + $4), updated_at = now()
		WHERE day = $1 AND actor IN ($2, $3)`, b.day(), b.databaseActor, actor, delta)
	return err
}

// Settle trues a reservation up (or down) to the reported usage.
func (b *Budget) Settle(ctx context.Context, actor string, reserved, used int64) error {
	if err := checkActor(actor); err != nil {
		return err
	}
	if used < 0 || reserved < 0 {
		return fmt.Errorf("%w: settle %d of %d reserved tokens", ErrInvalid, used, reserved)
	}
	if used == reserved {
		return nil
	}
	err := pgx.BeginFunc(ctx, b.pool, func(tx pgx.Tx) error {
		if _, _, err := b.lockRows(ctx, tx, actor); err != nil {
			return err
		}
		return b.add(ctx, tx, actor, used-reserved)
	})
	return b.wrap(err, "settle")
}

// Status is today's usage of the database and actor.
func (b *Budget) Status(ctx context.Context, actor string) (BudgetStatus, error) {
	if err := checkActor(actor); err != nil {
		return BudgetStatus{}, err
	}
	st := BudgetStatus{Day: b.day().Format("2006-01-02"), DatabaseLimit: b.perDatabase,
		UserLimit: b.perUser}
	err := b.pool.QueryRow(ctx, `/* pg_sage */ SELECT
		COALESCE(max(tokens) FILTER (WHERE actor = $2), 0),
		COALESCE(max(tokens) FILTER (WHERE actor = $3), 0)
		FROM sage.ask_budget_day WHERE day = $1 AND actor IN ($2, $3)`, b.day(),
		b.databaseActor, actor).Scan(&st.DatabaseUsed, &st.UserUsed)
	return st, b.wrap(err, "read")
}

// wrap keeps a budget refusal as is and marks store failures unavailable.
func (b *Budget) wrap(err error, op string) error {
	var be *BudgetError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &be):
		return be
	case errors.Is(err, context.Canceled):
		return err
	}
	return fmt.Errorf("%w: %s the Ask Sage budget: %v", ErrUnavailable, op, err)
}
