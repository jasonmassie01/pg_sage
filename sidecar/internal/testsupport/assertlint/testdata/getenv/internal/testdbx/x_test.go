package testdbx

import (
	"os"
	"testing"
)

func TestX(t *testing.T) {
	if os.Getenv("SAGE_TEST_DATABASE_URL") == "" {
		t.Skip("no dsn")
	}
	t.Error("x")
}
