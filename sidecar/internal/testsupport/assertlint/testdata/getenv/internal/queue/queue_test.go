package queue

import (
	"os"
	"testing"

	tdb "example.com/fix/internal/testdb"
)

// openPool reads the DSN through testdb's constant: still a direct read.
func openPool(t *testing.T) string {
	if os.Getenv(tdb.EnvName) == "" {
		t.Fatal("no dsn")
	}
	return tdb.SkipUnlessLive(t)
}

// otherEnv reads an EnvName of a package that is not testdb.
func otherEnv() string { return os.Getenv(cfg.EnvName) }
