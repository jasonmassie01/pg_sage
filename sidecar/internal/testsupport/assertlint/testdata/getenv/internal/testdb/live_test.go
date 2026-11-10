package testdb

import (
	"os"
	"testing"
)

func TestLive(t *testing.T) {
	if os.Getenv("SAGE_TEST_DATABASE_URL") == "" {
		t.Skip("no dsn")
	}
}
