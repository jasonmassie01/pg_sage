package testdb

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

// exclusiveHolders maps each test holding the exclusive server (Exclusive)
// to that server's URL, until the test ends.
var exclusiveHolders sync.Map // testing.TB -> string

// ExclusiveDatabase creates one more fresh database on the exclusive
// server, for a test that already holds it through Exclusive and needs a
// second database there (a meta store next to the monitored database).
// It is dropped when the test ends, before the server is released.
func ExclusiveDatabase(t testing.TB, label string) string {
	t.Helper()
	dsn, err := exclusiveDatabase(t, label)
	if err != nil {
		t.Fatalf("exclusive-cluster: %v", err)
	}
	return dsn
}

func exclusiveDatabase(t testing.TB, label string) (string, error) {
	held, ok := exclusiveHolders.Load(t)
	if !ok {
		return "", errors.New("extra database on the exclusive server: " +
			"this test does not hold it; call Exclusive first")
	}
	base := held.(string)
	name, err := extraDatabaseName(label)
	if err != nil {
		return "", fmt.Errorf("name the extra exclusive database: %w", err)
	}
	dsn, err := createFixture(base, name, "exclusive")
	if err != nil {
		return "", fmt.Errorf("extra database on the exclusive server: %w", err)
	}
	t.Cleanup(func() {
		if err := dropFixture(base, name); err != nil {
			t.Errorf("exclusive-cluster: drop the extra exclusive database: %v", err)
		}
	})
	return dsn, nil
}
