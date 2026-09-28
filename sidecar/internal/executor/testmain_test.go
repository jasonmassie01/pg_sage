package executor

import (
	"os"
	"testing"

	"github.com/pg-sage/sidecar/internal/testdb"
)

func TestMain(m *testing.M) {
	// cgo-less local builds would queue every unattended mutation.
	AssumeASTValidationForTests()
	os.Exit(testdb.Run(m.Run, "internal/executor"))
}
