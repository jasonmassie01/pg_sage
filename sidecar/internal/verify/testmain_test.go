package verify

import (
	"os"
	"testing"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// TestMain gives the package a private fixture database so its
// integration tests bootstrap the current sage schema instead of relying
// on whatever schema the shared server's default database happens to hold.
func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "internal/verify"))
}
