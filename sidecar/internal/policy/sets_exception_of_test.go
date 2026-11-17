package policy

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// fleetpol-01c: ExceptionOf is one database's exception in force, the read
// the inbox makes per database. Its SQL runs in
// inbox.TestInboxDB_PolicyExceptionsAreItemsOfTheirDatabase on a real
// server. No concurrent access tests: a read-only snapshot, like Exceptions.

func TestExceptionOf_NoStoreIsUnavailable(t *testing.T) {
	var nilStore *Store
	got, err := nilStore.ExceptionOf(context.Background(), 9)
	if !errors.Is(err, ErrUnavailable) || got != nil {
		t.Fatalf("nil store: %+v, %v; want ErrUnavailable", got, err)
	}
	got, err = (&Store{}).ExceptionOf(context.Background(), 9)
	if !errors.Is(err, ErrUnavailable) || got != nil {
		t.Fatalf("store without a pool: %+v, %v; want ErrUnavailable", got, err)
	}
}

func TestExceptionOf_RefusesANonPositiveID(t *testing.T) {
	// A pool is needed to reach the id check (a store without one is
	// ErrUnavailable first). pgxpool.New does not connect, and the refusal
	// must come before any query, so nothing listens on this address.
	pool, err := pgxpool.New(context.Background(), "postgres://sage@127.0.0.1:1/none")
	if err != nil {
		t.Fatalf("lazy pool: %v", err)
	}
	defer pool.Close()
	for _, id := range []int64{0, -1} {
		got, err := NewStore(pool).ExceptionOf(context.Background(), id)
		if !errors.Is(err, ErrInvalidSet) || got != nil {
			t.Fatalf("id %d: %+v, %v; want ErrInvalidSet", id, got, err)
		}
	}
}
