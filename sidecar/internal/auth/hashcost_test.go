package auth

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

// The panic outside a test binary is not tested here: testing.Testing()
// is always true in this binary, so that branch cannot run in-process.

func hashCostOf(t *testing.T, password string) int {
	t.Helper()
	h, err := HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !CheckPassword(h, password) {
		t.Fatal("the hash does not check against its own password")
	}
	if CheckPassword(h, password+"x") {
		t.Fatal("the hash checks against a different password")
	}
	cost, err := bcrypt.Cost([]byte(h))
	if err != nil {
		t.Fatalf("bcrypt.Cost: %v", err)
	}
	return cost
}

// Without the test hook, new hashes use the production cost.
func TestHashPassword_UsesBcryptCostByDefault(t *testing.T) {
	if got := hashCostOf(t, "pw-default-cost"); got != BcryptCost {
		t.Fatalf("hash cost = %d, want BcryptCost %d", got, BcryptCost)
	}
}

// State transitions: lowered to bcrypt's minimum, then restored.
func TestUseMinimumHashCostForTests_LowersThenRestores(t *testing.T) {
	restore := UseMinimumHashCostForTests()
	if got := hashCostOf(t, "pw-min-cost"); got != bcrypt.MinCost {
		restore()
		t.Fatalf("hash cost = %d, want bcrypt.MinCost %d", got, bcrypt.MinCost)
	}
	restore()
	if got := int(hashCost.Load()); got != BcryptCost {
		t.Fatalf("after restore the cost is %d, want BcryptCost %d", got, BcryptCost)
	}
}

// A hash made at either cost checks after the cost changes: checks follow
// the cost recorded in each hash, so lowered suites still log users in.
func TestCheckPassword_FollowsEachHashsCost(t *testing.T) {
	restore := UseMinimumHashCostForTests()
	low, err := HashPassword("pw-low")
	restore()
	if err != nil {
		t.Fatalf("HashPassword at the minimum: %v", err)
	}
	if !CheckPassword(low, "pw-low") || CheckPassword(low, "pw-other") {
		t.Fatal("a minimum-cost hash did not check correctly at the production cost")
	}
}

// Invalid input and error propagation at the lowered cost: bcrypt's
// 72-byte limit still fails the hash with a distinguishable error, and an
// empty password still hashes and checks only against itself.
func TestUseMinimumHashCostForTests_KeepsBcryptErrors(t *testing.T) {
	restore := UseMinimumHashCostForTests()
	defer restore()
	_, err := HashPassword(strings.Repeat("p", 73))
	if !errors.Is(err, bcrypt.ErrPasswordTooLong) {
		t.Fatalf("73-byte password: err = %v, want bcrypt.ErrPasswordTooLong", err)
	}
	if !strings.Contains(err.Error(), "hashing password") {
		t.Fatalf("error lost its context: %v", err)
	}
	if got := hashCostOf(t, ""); got != bcrypt.MinCost {
		t.Fatalf("empty password hash cost = %d, want %d", got, bcrypt.MinCost)
	}
}

// Concurrent access: tests hash in parallel goroutines while a test
// changes the cost (run with -race).
func TestUseMinimumHashCostForTests_ConcurrentHashing(t *testing.T) {
	restore := UseMinimumHashCostForTests()
	defer restore()
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if _, err := HashPassword("pw-concurrent"); err != nil {
				errs <- err
			}
		}()
		go func() {
			defer wg.Done()
			UseMinimumHashCostForTests()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent HashPassword: %v", err)
	}
	if got := int(hashCost.Load()); got != bcrypt.MinCost {
		t.Fatalf("cost after concurrent lowering = %d, want %d", got, bcrypt.MinCost)
	}
}
