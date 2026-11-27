package api

import (
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// apiauth-06: a full limiter never evicts a locked entry (that would give
// the account fresh guesses), fails closed when every entry is locked, and
// counts each such refusal.

// fillLimiter adds n distinct keys with attempts each, the newest at last.
func fillLimiter(l *loginRateLimiter, n, attempts int, last time.Time) {
	for i := 0; i < n; i++ {
		ts := make([]time.Time, attempts)
		for j := range ts {
			ts[j] = last.Add(time.Duration(j-attempts+1) * time.Millisecond)
		}
		l.attempts["filler"+strconv.Itoa(i)+"@example.com"] = ts
	}
}

func lockedAttempts(newest time.Time, n int) []time.Time {
	ts := make([]time.Time, n)
	for i := range ts {
		ts[i] = newest.Add(time.Duration(i-n+1) * time.Second)
	}
	return ts
}

func TestLoginLimiter_FullLimiterKeepsLockedEntry(t *testing.T) {
	l := newTestLimiter()
	now := time.Now()
	// The locked victim has the oldest newest attempt, so an eviction that
	// ignores the lock would pick it first.
	l.attempts["victim@example.com"] = lockedAttempts(
		now.Add(-10*time.Minute), loginMaxAttempts)
	fillLimiter(l, loginMaxEntries-1, 1, now)

	if !l.reserve("newcomer@example.com") {
		t.Fatal("new key refused although unlocked entries could be evicted")
	}
	if got := len(l.attempts); got != loginMaxEntries {
		t.Fatalf("tracked keys = %d, want %d", got, loginMaxEntries)
	}
	if l.reserve("victim@example.com") {
		t.Fatal("locked account got a fresh attempt after eviction")
	}
	if got := l.saturated.Load(); got != 0 {
		t.Fatalf("saturated = %d, want 0 when an unlocked entry was evictable", got)
	}
}

func TestLoginLimiter_AllLockedFailsClosedAndCounts(t *testing.T) {
	l := newTestLimiter()
	fillLimiter(l, loginMaxEntries, loginMaxAttempts, time.Now())

	if l.reserve("newcomer@example.com") {
		t.Fatal("new key admitted although every entry is locked")
	}
	if _, tracked := l.attempts["newcomer@example.com"]; tracked {
		t.Fatal("refused key was inserted")
	}
	if got := len(l.attempts); got != loginMaxEntries {
		t.Fatalf("tracked keys = %d, want %d (nothing evicted)", got, loginMaxEntries)
	}
	if got := l.saturated.Load(); got != 1 {
		t.Fatalf("saturated = %d, want 1", got)
	}
	if l.reserve("filler0@example.com") {
		t.Fatal("a locked filler became admissible")
	}
	if got := l.saturated.Load(); got != 1 {
		t.Fatalf("saturated = %d after a known-key refusal, want still 1", got)
	}
}

func TestLoginLimiter_EvictionPrefersFewestLiveAttempts(t *testing.T) {
	l := newTestLimiter()
	now := time.Now()
	// Four of five guesses spent, and the oldest newest attempt: evicting it
	// would hand the attacker four more guesses.
	l.attempts["almost@example.com"] = lockedAttempts(
		now.Add(-10*time.Minute), loginMaxAttempts-1)
	fillLimiter(l, loginMaxEntries-1, 1, now)

	if !l.reserve("newcomer@example.com") {
		t.Fatal("new key refused")
	}
	if got := len(l.attempts["almost@example.com"]); got != loginMaxAttempts-1 {
		t.Fatalf("nearly locked entry has %d attempts, want %d (it was evicted)",
			got, loginMaxAttempts-1)
	}
	if !l.reserve("almost@example.com") || l.reserve("almost@example.com") {
		t.Fatal("nearly locked entry did not keep its remaining budget of one")
	}
}

func TestLoginLimiter_EvictionPrefersExpiredEntries(t *testing.T) {
	l := newTestLimiter()
	now := time.Now()
	fillLimiter(l, loginMaxEntries-1, loginMaxAttempts, now)
	l.attempts["stale@example.com"] = lockedAttempts(
		now.Add(-2*loginWindow), loginMaxAttempts)

	if !l.reserve("newcomer@example.com") {
		t.Fatal("new key refused although an expired entry was evictable")
	}
	if _, tracked := l.attempts["stale@example.com"]; tracked {
		t.Fatal("expired entry survived the eviction")
	}
	if got := l.saturated.Load(); got != 0 {
		t.Fatalf("saturated = %d, want 0", got)
	}
}

func TestLoginLimiter_ThousandParallelReservationsAdmitExactlyFive(t *testing.T) {
	l := newTestLimiter()
	var granted atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 1000; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if l.reserve("victim@example.com") {
				granted.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if got := granted.Load(); got != loginMaxAttempts {
		t.Fatalf("granted = %d, want exactly %d", got, loginMaxAttempts)
	}
	if got := len(l.attempts["victim@example.com"]); got != loginMaxAttempts {
		t.Fatalf("recorded attempts = %d, want %d", got, loginMaxAttempts)
	}
}

func TestLoginLimiter_ZeroLimitMeansDefault(t *testing.T) {
	l := &loginRateLimiter{attempts: map[string][]time.Time{}}
	for i := 0; i < loginMaxAttempts; i++ {
		if !l.reserve("zero@example.com") {
			t.Fatalf("reservation %d refused under the default limit", i+1)
		}
	}
	if l.reserve("zero@example.com") {
		t.Fatalf("reservation %d admitted, want the default cap %d",
			loginMaxAttempts+1, loginMaxAttempts)
	}
}

func TestLoginFailureBudget_CapAndRefund(t *testing.T) {
	b := newTestFailureBudget()
	for i := 0; i < loginFailureBudgetLimit; i++ {
		if !b.reserve("192.0.2.1") {
			t.Fatalf("failure %d refused below the budget", i+1)
		}
	}
	if b.reserve("192.0.2.1") {
		t.Fatalf("failure %d admitted, want cap %d",
			loginFailureBudgetLimit+1, loginFailureBudgetLimit)
	}
	b.refund("192.0.2.1")
	if got := len(b.attempts["192.0.2.1"]); got != loginFailureBudgetLimit-1 {
		t.Fatalf("attempts after refund = %d, want %d", got, loginFailureBudgetLimit-1)
	}
	if !b.reserve("192.0.2.1") {
		t.Fatal("refund did not free one slot")
	}
	if b.reserve("192.0.2.1") {
		t.Fatal("refund freed more than one slot")
	}
}

func TestLoginFailureBudget_RefundOfUnknownKeyIsNoOp(t *testing.T) {
	b := newTestFailureBudget()
	b.refund("")
	b.refund("198.51.100.1")
	if got := len(b.attempts); got != 0 {
		t.Fatalf("refund created %d entries, want 0", got)
	}
	b.reserve("198.51.100.2")
	b.refund("198.51.100.2")
	if _, tracked := b.attempts["198.51.100.2"]; tracked {
		t.Fatal("refunding the only attempt left an empty entry behind")
	}
}

func TestLoginLimiterSaturatedTotalSumsBothLimiters(t *testing.T) {
	before := LoginLimiterSaturatedTotal()
	loginLimiter.saturated.Add(2)
	loginFailureBudget.saturated.Add(3)
	t.Cleanup(func() {
		loginLimiter.saturated.Add(^uint64(1))
		loginFailureBudget.saturated.Add(^uint64(2))
	})
	if got := LoginLimiterSaturatedTotal(); got != before+5 {
		t.Fatalf("saturated total = %d, want %d", got, before+5)
	}
}

func newTestFailureBudget() *loginRateLimiter {
	return &loginRateLimiter{
		attempts: make(map[string][]time.Time),
		limit:    loginFailureBudgetLimit,
	}
}
