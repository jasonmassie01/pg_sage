package store

import (
	"os"
	"testing"
)

var pkgDSN = os.Getenv("SAGE_TEST_DATABASE_URL")

func dsn() string {
	return os.Getenv("SAGE_TEST_DATABASE_URL")
}

func other() string { return os.Getenv("SAGE_DATABASE_URL") }

func TestStore(t *testing.T) {
	if _, ok := os.LookupEnv("SAGE_TEST_DATABASE_URL"); !ok {
		t.Skip("no dsn")
	}
	if dsn() == other() {
		t.Fatal("same")
	}
}
