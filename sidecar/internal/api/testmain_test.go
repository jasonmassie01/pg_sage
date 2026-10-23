package api

import (
	"os"
	"testing"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/testdb"
)

func TestMain(m *testing.M) {
	// cgo-less local builds would queue every unattended mutation.
	executor.AssumeASTValidationForTests()
	// Cost-12 bcrypt takes seconds per user under -race (hashcost_test.go).
	auth.UseMinimumHashCostForTests()
	os.Exit(testdb.Run(m.Run, "internal/api"))
}
