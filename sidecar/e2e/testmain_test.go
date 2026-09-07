//go:build e2e

package e2e

import (
	"os"
	"testing"

	"github.com/pg-sage/sidecar/internal/testdb"
)

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "e2e"))
}
