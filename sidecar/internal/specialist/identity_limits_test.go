package specialist

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/mcptoken"
)

// Identity: each external system is a named agent identity (an MCP token);
// its actor names it in the audit trail. Limits: per-identity rate limits.

func TestIdentityFromGrant(t *testing.T) {
	id := IdentityFromGrant(mcptoken.Grant{TokenID: "0b1c2d3e-0000-4000-8000-000000000001",
		Name: "PagerDuty (prod)", Kind: mcptoken.KindAgent,
		Scopes: []string{"read", "propose"}, Databases: []string{"orders"}}, "http")
	if id.Name != "PagerDuty (prod)" || id.Transport != "http" || id.Kind != "agent" {
		t.Fatalf("identity %+v", id)
	}
	if got := id.Actor(); got != "agent:pagerduty-prod:0b1c2d3e-0000-4000-8000-000000000001" {
		t.Fatalf("actor = %q", got)
	}
	if !id.Has(ScopeRead) || !id.Has(ScopePropose) || id.Has("approve") {
		t.Fatalf("scopes: read/propose held, approve never: %+v", id)
	}
	if !id.MayUse("orders") || id.MayUse("billing") || id.MayUse("") {
		t.Fatal("database restriction")
	}
}

func TestIdentity_ApproveIsNeverHeld(t *testing.T) {
	// Even an operator token carrying approve cannot approve through the
	// specialist contract: there is nothing to approve here.
	id := IdentityFromGrant(mcptoken.Grant{TokenID: "t1", Name: "ops",
		Kind: mcptoken.KindOperator, Scopes: []string{"read", "propose", "approve"}}, "http")
	if id.Has("approve") {
		t.Fatal("approve must never be held through the specialist contract")
	}
	if !id.MayUse("anything") {
		t.Fatal("nil databases means every database")
	}
	if !strings.HasPrefix(id.Actor(), "token:ops:") {
		t.Fatalf("operator token actor = %q", id.Actor())
	}
}

func TestIdentity_ZeroValueHoldsNothing(t *testing.T) {
	var id Identity
	if id.Has(ScopeRead) || id.Has(ScopePropose) {
		t.Fatal("an identity without a token holds no scope")
	}
	id = Identity{TokenID: "x", Scopes: []string{"read"}, Databases: []string{}}
	if id.MayUse("orders") {
		t.Fatal("an empty (non-nil) database list permits nothing")
	}
}

func TestIdentity_ActorIsBoundedAndPrintable(t *testing.T) {
	id := Identity{TokenID: "tok", Kind: "agent",
		Name: strings.Repeat("Ünïcödé\x07 ", 40)}
	actor := id.Actor()
	if len(actor) > 128 || strings.ContainsAny(actor, "\x07 ") {
		t.Fatalf("actor %q", actor)
	}
	if got := (Identity{TokenID: "tok", Kind: "agent", Name: "!!!"}).Actor(); got !=
		"agent:unnamed:tok" {
		t.Fatalf("empty slug actor = %q", got)
	}
}

func TestLimits_Validate(t *testing.T) {
	if err := DefaultLimits().Validate(); err != nil {
		t.Fatal(err)
	}
	d := DefaultLimits()
	if d.WritesPerMinute != 30 || d.ReadsPerMinute != 240 || d.MaxOpenPerIdentity != 3 ||
		d.MaxOpenTotal != 10 {
		t.Fatalf("defaults %+v", d)
	}
	for _, bad := range []Limits{{0, 1, 1, 1}, {1, 0, 1, 1}, {1, 1, 0, 1}, {1, 1, 1, 0},
		{-1, 1, 1, 1}, {1, 1, 5, 4}, {100001, 1, 1, 1}} {
		if err := bad.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v: want ErrInvalid, got %v", bad, err)
		}
	}
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func TestRateLimiter_BoundaryAndRefill(t *testing.T) {
	clock := &fakeClock{now: validateNow}
	rl := newRateLimiter(6, clock.Now) // one token every 10 s, burst 6
	for i := 0; i < 6; i++ {
		if ok, _ := rl.Allow("a"); !ok {
			t.Fatalf("call %d within the burst refused", i+1)
		}
	}
	ok, retry := rl.Allow("a")
	if ok || retry <= 0 || retry > 10*time.Second {
		t.Fatalf("7th call: ok=%t retry=%s", ok, retry)
	}
	clock.Advance(9 * time.Second)
	if ok, _ := rl.Allow("a"); ok {
		t.Fatal("a token needs 10 s")
	}
	clock.Advance(time.Second)
	if ok, _ := rl.Allow("a"); !ok {
		t.Fatal("refilled after 10 s")
	}
	// Identities are isolated.
	if ok, _ := rl.Allow("b"); !ok {
		t.Fatal("another identity has its own bucket")
	}
	// Refill never exceeds the burst.
	clock.Advance(time.Hour)
	allowed := 0
	for i := 0; i < 20; i++ {
		if ok, _ := rl.Allow("a"); ok {
			allowed++
		}
	}
	if allowed != 6 {
		t.Fatalf("after an hour idle the burst is 6, got %d", allowed)
	}
}

func TestRateLimiter_ConcurrentCallsNeverExceedTheBurst(t *testing.T) {
	clock := &fakeClock{now: validateNow}
	rl := newRateLimiter(50, clock.Now)
	var granted atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, _ := rl.Allow("shared"); ok {
				granted.Add(1)
			}
		}()
	}
	wg.Wait()
	if granted.Load() != 50 {
		t.Fatalf("granted %d of a burst of 50", granted.Load())
	}
}

func TestRateLimiter_BoundsIdleKeys(t *testing.T) {
	clock := &fakeClock{now: validateNow}
	rl := newRateLimiter(10, clock.Now)
	for i := 0; i < maxLimiterKeys+500; i++ {
		rl.Allow(fmt.Sprintf("k%d", i))
		if i%1000 == 0 {
			clock.Advance(time.Minute)
		}
	}
	if n := rl.size(); n > maxLimiterKeys {
		t.Fatalf("limiter holds %d keys over the bound %d", n, maxLimiterKeys)
	}
}
