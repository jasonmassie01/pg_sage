package auth

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

// apiauth-06: accounts without a password hash pay a bcrypt comparison at
// the active cost, so response time does not reveal which emails exist.

func TestTimingEqualizerHash_FollowsActiveCost(t *testing.T) {
	restore := UseMinimumHashCostForTests()
	minHash, err := timingEqualizerHash()
	if err != nil {
		restore()
		t.Fatalf("minimum-cost equaliser hash: %v", err)
	}
	if got, _ := bcrypt.Cost(minHash); got != bcrypt.MinCost {
		restore()
		t.Fatalf("equaliser cost = %d under the test cost, want %d", got, bcrypt.MinCost)
	}
	restore()
	prodHash, err := timingEqualizerHash()
	if err != nil {
		t.Fatalf("production-cost equaliser hash: %v", err)
	}
	if got, _ := bcrypt.Cost(prodHash); got != BcryptCost {
		t.Fatalf("equaliser cost = %d after restore, want %d", got, BcryptCost)
	}
	again, err := timingEqualizerHash()
	if err != nil || string(again) != string(prodHash) {
		t.Fatalf("second call rebuilt the hash (err=%v): it must be cached", err)
	}
}

func TestTimingEqualizerHash_ConcurrentFirstUseAgreesOnCost(t *testing.T) {
	restore := UseMinimumHashCostForTests()
	defer restore()
	var wg sync.WaitGroup
	errs := make(chan error, 50)
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h, err := timingEqualizerHash()
			if err != nil {
				errs <- err
				return
			}
			if c, _ := bcrypt.Cost(h); c != bcrypt.MinCost {
				errs <- errors.New("equaliser hash at the wrong cost")
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

func TestAuthenticate_DBErrorIsNotInvalidCredentials(t *testing.T) {
	pool, err := pgxpool.New(context.Background(),
		"postgres://sage:unused@127.0.0.1:1/sage?connect_timeout=1")
	if err != nil {
		t.Fatalf("lazy pool: %v", err)
	}
	defer pool.Close()
	u, err := Authenticate(context.Background(), pool, "x@fixture.invalid", "pw")
	if u != nil {
		t.Fatalf("user = %+v on a DB error, want nil", u)
	}
	if err == nil || !strings.Contains(err.Error(), "querying user") {
		t.Fatalf("err = %v, want it to name %q", err, "querying user")
	}
	if errors.Is(err, ErrInvalidCredentials) {
		t.Fatal("a DB failure was reported as invalid credentials")
	}
}
