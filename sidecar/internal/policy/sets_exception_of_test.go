package policy

import (
	"context"
	"errors"
	"testing"
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
	for _, id := range []int64{0, -1} {
		got, err := (&Store{}).ExceptionOf(context.Background(), id)
		if err == nil || got != nil {
			t.Fatalf("id %d: %+v, %v; want an error", id, got, err)
		}
	}
}
