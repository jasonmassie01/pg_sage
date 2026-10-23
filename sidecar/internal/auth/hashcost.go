package auth

import (
	"sync/atomic"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

// hashCost is the bcrypt cost of new password hashes: BcryptCost, except
// in a test binary that lowered it with UseMinimumHashCostForTests.
var hashCost atomic.Int32

func init() { hashCost.Store(BcryptCost) }

// inTestBinary reports whether this is a test binary (a var so the
// production refusal can be tested in-process).
var inTestBinary = testing.Testing

// UseMinimumHashCostForTests makes HashPassword use bcrypt's minimum cost
// and returns a func that restores BcryptCost. Under the race detector a
// cost-12 hash or check takes seconds, so suites that create many users
// call it from TestMain; CheckPassword follows each hash's own cost. It
// panics outside a test binary: production hashes always use BcryptCost.
func UseMinimumHashCostForTests() (restore func()) {
	if !inTestBinary() {
		panic("auth: UseMinimumHashCostForTests called outside a test binary")
	}
	hashCost.Store(int32(bcrypt.MinCost))
	return func() { hashCost.Store(BcryptCost) }
}
