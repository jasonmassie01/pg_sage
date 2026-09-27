package fleet

import (
	"os"
	"testing"

	"github.com/pg-sage/sidecar/internal/executor"
)

func TestMain(m *testing.M) {
	// cgo-less local builds would queue every unattended mutation.
	executor.AssumeASTValidationForTests()
	os.Exit(m.Run())
}
