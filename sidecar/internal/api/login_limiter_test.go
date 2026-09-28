package api

import (
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// SURF-16 / G6-B06: capacity holds on every insertion path, attempts
// are reserved atomically, and identities are normalised.

func newTestLimiter() *loginRateLimiter {
	return &loginRateLimiter{attempts: make(map[string][]time.Time)}
}

func TestLoginLimiter_CapacityBoundedForDistinctEmails(t *testing.T) {
	l := newTestLimiter()
	for i := 0; i <= loginMaxEntries+50; i++ {
		l.reserve("spray" + strconv.Itoa(i) + "@example.com")
	}
	if got := len(l.attempts); got > loginMaxEntries {
		t.Fatalf("tracked emails = %d, want <= %d", got, loginMaxEntries)
	}
}

func TestLoginLimiter_ConcurrentReservationsNeverExceedLimit(t *testing.T) {
	l := newTestLimiter()
	var granted atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if l.reserve("victim@example.com") {
				granted.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := granted.Load(); got != loginMaxAttempts {
		t.Fatalf("granted = %d, want exactly %d", got, loginMaxAttempts)
	}
}

func TestLoginLimiter_NormalisesEmail(t *testing.T) {
	l := newTestLimiter()
	for i := 0; i < loginMaxAttempts; i++ {
		if !l.reserve("Admin@Example.com ") {
			t.Fatalf("reservation %d refused early", i)
		}
	}
	if l.reserve("admin@example.com") {
		t.Fatal("case/whitespace variant bypassed the limit")
	}
	l.reset("ADMIN@example.com")
	if !l.reserve("admin@example.com") {
		t.Fatal("reset did not clear the normalised bucket")
	}
}

func TestLoginLimiter_ExpiredAttemptsFreeCapacity(t *testing.T) {
	l := newTestLimiter()
	old := time.Now().Add(-2 * loginWindow)
	l.attempts["victim@example.com"] = []time.Time{old, old, old, old, old}
	if !l.reserve("victim@example.com") {
		t.Fatal("expired attempts still counted")
	}
	if got := len(l.attempts["victim@example.com"]); got != 1 {
		t.Fatalf("attempts = %d, want 1 after pruning", got)
	}
}
