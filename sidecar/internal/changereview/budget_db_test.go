package changereview

import (
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// CHECK-FND-05: the persisted daily budget reserves against the database
// total ('*') and the actor in one row-locked transaction, refuses with a
// typed BudgetError, rolls over at the UTC day and settles to the usage
// the provider reported.

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

// today is a reservation of tokens by actor on the current UTC day.
func today(actor string, tokens int64) Reservation {
	t := time.Now().UTC()
	return Reservation{Day: time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC),
		Actor: actor, Tokens: tokens}
}

func reserveErr(f *fixture, b *Budget, actor string, tokens int64) error {
	_, err := b.Reserve(f.ctx, actor, tokens)
	return err
}

func TestBudgetDatabaseCapUnderConcurrency(t *testing.T) {
	f := newFixture(t)
	c := &clock{t: time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)}
	b := NewBudget(f.pool, 100000, 100000, c.now)
	var ok, refused atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := b.Reserve(f.ctx, "actor:"+strconv.Itoa(i), 10000)
			var be *BudgetError
			switch {
			case err == nil:
				ok.Add(1)
			case errors.As(err, &be) && be.Scope == BudgetScopeDatabase:
				refused.Add(1)
			default:
				t.Errorf("Reserve: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if ok.Load() != 10 || refused.Load() != 10 {
		t.Fatalf("%d reserved, %d refused; want exactly 10 and 10", ok.Load(), refused.Load())
	}
	if n := f.count(`SELECT tokens FROM sage.change_review_budget_day
		WHERE day = '2026-10-22' AND actor = '*'`); n != 100000 {
		t.Fatalf("the database row reads %d, want 100000", n)
	}
}

func TestBudgetActorCap(t *testing.T) {
	f := newFixture(t)
	b := NewBudget(f.pool, 500000, 200000, nil)
	if _, err := b.Reserve(f.ctx, "user:1", 200000); err != nil {
		t.Fatalf("reserve to the cap: %v", err)
	}
	_, err := b.Reserve(f.ctx, "user:1", 1)
	var be *BudgetError
	if !errors.As(err, &be) || be.Scope != BudgetScopeActor || be.Used != 200000 ||
		be.Limit != 200000 || be.Need != 1 || !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("actor over its cap: %v (%+v)", err, be)
	}
	if _, err := b.Reserve(f.ctx, "user:2", 50000); err != nil {
		t.Fatalf("another actor refused: %v", err)
	}
	st, err := b.Status(f.ctx, "user:2")
	if err != nil || st.DatabaseUsed != 250000 || st.ActorUsed != 50000 ||
		st.DatabaseLimit != 500000 || st.ActorLimit != 200000 {
		t.Fatalf("status = %+v, %v", st, err)
	}
}

func TestBudgetDayRollsOver(t *testing.T) {
	f := newFixture(t)
	c := &clock{t: time.Date(2026, 10, 22, 23, 59, 0, 0, time.UTC)}
	b := NewBudget(f.pool, 1000, 1000, c.now)
	if _, err := b.Reserve(f.ctx, "user:1", 1000); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Reserve(f.ctx, "user:1", 1); err == nil {
		t.Fatal("over the cap the same day was accepted")
	}
	c.set(time.Date(2026, 10, 23, 0, 0, 1, 0, time.UTC))
	if _, err := b.Reserve(f.ctx, "user:1", 1000); err != nil {
		t.Fatalf("a new UTC day still refused: %v", err)
	}
	st, _ := b.Status(f.ctx, "user:1")
	if st.Day != "2026-10-23" || st.DatabaseUsed != 1000 {
		t.Fatalf("status after rollover = %+v", st)
	}
	// A clock in another zone still uses the UTC day.
	c.set(time.Date(2026, 10, 23, 20, 0, 0, 0, time.FixedZone("x", -6*3600)))
	if st, _ := b.Status(f.ctx, "user:1"); st.Day != "2026-10-24" {
		t.Fatalf("the budget day %s is not the UTC day", st.Day)
	}
}

func TestBudgetSettleRefundsAndCharges(t *testing.T) {
	f := newFixture(t)
	b := NewBudget(f.pool, 100000, 100000, nil)
	if _, err := b.Reserve(f.ctx, "user:1", 3000); err != nil {
		t.Fatal(err)
	}
	if err := b.Settle(f.ctx, today("user:1", 3000), 1200); err != nil {
		t.Fatalf("settle: %v", err)
	}
	st, _ := b.Status(f.ctx, "user:1")
	if st.DatabaseUsed != 1200 || st.ActorUsed != 1200 {
		t.Fatalf("after a refund: %+v, want 1200 used", st)
	}
	if err := b.Settle(f.ctx, today("user:1", 1000), 1500); err != nil {
		t.Fatal(err)
	}
	if st, _ := b.Status(f.ctx, "user:1"); st.ActorUsed != 1700 {
		t.Fatalf("after an overrun: %+v, want 1700", st)
	}
	if err := b.Settle(f.ctx, today("user:1", 99999), 0); err != nil {
		t.Fatal(err)
	}
	if st, _ := b.Status(f.ctx, "user:1"); st.ActorUsed != 0 || st.DatabaseUsed != 0 {
		t.Fatalf("a refund larger than the usage went below zero: %+v", st)
	}
}

func TestBudgetInvalidArguments(t *testing.T) {
	f := newFixture(t)
	b := NewBudget(f.pool, 1000, 1000, nil)
	for name, err := range map[string]error{
		"zero tokens":     reserveErr(f, b, "user:1", 0),
		"negative tokens": reserveErr(f, b, "user:1", -5),
		"star actor":      reserveErr(f, b, "*", 10),
		"empty actor":     reserveErr(f, b, " ", 10),
		"negative settle": b.Settle(f.ctx, today("user:1", 10), -1),
	} {
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
	if _, err := b.Status(f.ctx, ""); !errors.Is(err, ErrInvalid) {
		t.Errorf("status of an empty actor: %v", err)
	}
	if err := b.Settle(f.ctx, today("user:1", 10), 10); err != nil {
		t.Errorf("settle with no change: %v", err)
	}
}

func TestBudgetZeroCapRefusesEverything(t *testing.T) {
	f := newFixture(t)
	b := NewBudget(f.pool, 0, 0, nil)
	var be *BudgetError
	if _, err := b.Reserve(f.ctx, "user:1", 1); !errors.As(err, &be) ||
		be.Scope != BudgetScopeDatabase {
		t.Fatalf("zero cap: %v", err)
	}
}

func TestBudgetStoreFailureIsUnavailable(t *testing.T) {
	f := newFixture(t)
	b := NewBudget(f.pool, 1000, 1000, nil)
	f.pool.Close()
	if _, err := b.Reserve(f.ctx, "user:1", 10); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("a closed pool gives %v, want ErrUnavailable", err)
	}
}
