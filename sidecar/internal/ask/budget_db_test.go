package ask

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Ask Sage's own LLM budget (owner decision 4): a daily token allocation
// per database and per user, persisted in sage.ask_budget_day so it
// survives restarts, separate from the investigator's and the tuning
// agent's budgets. A call is reserved before it is sent and settled with
// the usage the provider reported.

var budgetDay = time.Date(2026, 10, 4, 15, 0, 0, 0, time.UTC)

func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }

func newBudget(f *fixture, perDB, perUser int64) *Budget {
	return NewBudget(f.pool, perDB, perUser, fixedClock(budgetDay))
}

func budgetErr(t *testing.T, err error) *BudgetError {
	t.Helper()
	var be *BudgetError
	if !errors.Is(err, ErrBudgetExhausted) || !errors.As(err, &be) {
		t.Fatalf("err = %v, want a *BudgetError wrapping ErrBudgetExhausted", err)
	}
	return be
}

func TestBudget_ReserveChargesTheUserAndTheDatabase(t *testing.T) {
	f := newFixture(t)
	b := newBudget(f, 10_000, 4_000)
	if err := b.Reserve(f.ctx, "user:1", 1_500); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := b.Reserve(f.ctx, "user:2", 2_000); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	st, err := b.Status(f.ctx, "user:1")
	if err != nil {
		t.Fatal(err)
	}
	want := BudgetStatus{Day: "2026-10-04", DatabaseUsed: 3_500, DatabaseLimit: 10_000,
		UserUsed: 1_500, UserLimit: 4_000}
	if st != want {
		t.Fatalf("status = %+v, want %+v", st, want)
	}
}

func TestBudget_UserCapRefusesOnlyThatUser(t *testing.T) {
	f := newFixture(t)
	b := newBudget(f, 10_000, 4_000)
	if err := b.Reserve(f.ctx, "user:1", 3_000); err != nil {
		t.Fatal(err)
	}
	be := budgetErr(t, b.Reserve(f.ctx, "user:1", 1_001))
	if be.Scope != BudgetScopeUser || be.Used != 3_000 || be.Limit != 4_000 ||
		be.Need != 1_001 {
		t.Fatalf("budget error = %+v", be)
	}
	if err := b.Reserve(f.ctx, "user:2", 3_000); err != nil {
		t.Fatalf("another user refused: %v", err)
	}
	st, _ := b.Status(f.ctx, "user:1")
	if st.UserUsed != 3_000 || st.DatabaseUsed != 6_000 {
		t.Fatalf("a refused reservation was charged: %+v", st)
	}
}

func TestBudget_DatabaseCapRefusesEveryone(t *testing.T) {
	f := newFixture(t)
	b := newBudget(f, 5_000, 4_000)
	for _, u := range []string{"user:1", "user:2"} {
		if err := b.Reserve(f.ctx, u, 2_500); err != nil {
			t.Fatal(err)
		}
	}
	be := budgetErr(t, b.Reserve(f.ctx, "user:3", 1))
	if be.Scope != BudgetScopeDatabase || be.Used != 5_000 || be.Limit != 5_000 {
		t.Fatalf("budget error = %+v", be)
	}
}

func TestBudget_ExactCapIsAllowedOneMoreIsNot(t *testing.T) {
	f := newFixture(t)
	b := newBudget(f, 1_000, 1_000)
	if err := b.Reserve(f.ctx, "user:1", 1_000); err != nil {
		t.Fatalf("reserving exactly the cap refused: %v", err)
	}
	budgetErr(t, b.Reserve(f.ctx, "user:1", 1))
}

func TestBudget_ZeroAllocationRefusesEveryCall(t *testing.T) {
	f := newFixture(t)
	be := budgetErr(t, newBudget(f, 0, 0).Reserve(f.ctx, "user:1", 1))
	if be.Scope != BudgetScopeDatabase {
		t.Fatalf("scope = %s, want database (checked first)", be.Scope)
	}
}

func TestBudget_SettleTrueUpsToReportedUsage(t *testing.T) {
	f := newFixture(t)
	b := newBudget(f, 100_000, 50_000)
	steps := []struct{ reserved, used, wantUser int64 }{
		{4_000, 1_200, 1_200}, // used less: the rest is released
		{2_000, 0, 1_200},     // provider failure: everything released
		{1_000, 3_000, 4_200}, // used more than reserved: charged truthfully
	}
	for _, s := range steps {
		if err := b.Reserve(f.ctx, "user:1", s.reserved); err != nil {
			t.Fatal(err)
		}
		if err := b.Settle(f.ctx, "user:1", s.reserved, s.used); err != nil {
			t.Fatal(err)
		}
		st, _ := b.Status(f.ctx, "user:1")
		if st.UserUsed != s.wantUser || st.DatabaseUsed != s.wantUser {
			t.Fatalf("after reserve %d used %d: %+v, want %d", s.reserved, s.used, st,
				s.wantUser)
		}
	}
}

func TestBudget_PersistsAcrossRestarts(t *testing.T) {
	f := newFixture(t)
	if err := newBudget(f, 10_000, 5_000).Reserve(f.ctx, "user:1", 4_500); err != nil {
		t.Fatal(err)
	}
	restarted := newBudget(f, 10_000, 5_000)
	budgetErr(t, restarted.Reserve(f.ctx, "user:1", 600))
	st, _ := restarted.Status(f.ctx, "user:1")
	if st.UserUsed != 4_500 {
		t.Fatalf("restarted budget forgot usage: %+v", st)
	}
}

func TestBudget_NewUTCDayStartsFresh(t *testing.T) {
	f := newFixture(t)
	if err := newBudget(f, 1_000, 1_000).Reserve(f.ctx, "user:1", 1_000); err != nil {
		t.Fatal(err)
	}
	// 23:59 the same UTC day is still exhausted; 00:00 the next day is not.
	late := NewBudget(f.pool, 1_000, 1_000, fixedClock(time.Date(2026, 10, 4, 23, 59, 0, 0,
		time.UTC)))
	budgetErr(t, late.Reserve(f.ctx, "user:1", 1))
	next := NewBudget(f.pool, 1_000, 1_000, fixedClock(time.Date(2026, 10, 5, 0, 0, 0, 0,
		time.UTC)))
	if err := next.Reserve(f.ctx, "user:1", 1_000); err != nil {
		t.Fatalf("next day refused: %v", err)
	}
	st, _ := next.Status(f.ctx, "user:1")
	if st.Day != "2026-10-05" || st.UserUsed != 1_000 {
		t.Fatalf("next day status = %+v", st)
	}
}

func TestBudget_InvalidInput(t *testing.T) {
	f := newFixture(t)
	b := newBudget(f, 1_000, 1_000)
	for _, actor := range []string{"", "   ", "*", strings.Repeat("a", 201)} {
		if err := b.Reserve(f.ctx, actor, 10); !errors.Is(err, ErrInvalid) {
			t.Errorf("actor %q: err = %v, want ErrInvalid", actor, err)
		}
	}
	for _, n := range []int64{0, -1} {
		if err := b.Reserve(f.ctx, "user:1", n); !errors.Is(err, ErrInvalid) {
			t.Errorf("tokens %d: err = %v, want ErrInvalid", n, err)
		}
	}
	if err := b.Settle(f.ctx, "user:1", 10, -1); !errors.Is(err, ErrInvalid) {
		t.Errorf("negative usage: err = %v", err)
	}
	if n := f.count(`SELECT count(*) FROM sage.ask_budget_day`); n != 0 {
		t.Fatalf("invalid calls wrote %d budget rows", n)
	}
}

func TestBudget_ConcurrentReservationsNeverOverspend(t *testing.T) {
	f := newFixture(t)
	b := newBudget(f, 1_000_000, 1_000)
	var ok, refused atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := b.Reserve(f.ctx, "user:1", 100)
			switch {
			case err == nil:
				ok.Add(1)
			case errors.Is(err, ErrBudgetExhausted):
				refused.Add(1)
			default:
				t.Errorf("reserve: %v", err)
			}
		}()
	}
	wg.Wait()
	st, _ := b.Status(f.ctx, "user:1")
	if ok.Load() != 10 || refused.Load() != 10 || st.UserUsed != 1_000 {
		t.Fatalf("ok %d refused %d used %d, want 10/10/1000", ok.Load(), refused.Load(),
			st.UserUsed)
	}
}

func TestBudget_StoreFailureIsUnavailableNotExhausted(t *testing.T) {
	f := newFixture(t)
	closed, err := pgxpool.New(f.ctx, f.pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	closed.Close()
	err = NewBudget(closed, 1_000, 1_000, fixedClock(budgetDay)).Reserve(f.ctx, "user:1", 1)
	if !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("closed pool: err = %v, want ErrUnavailable only", err)
	}
}
