package cases_test

import (
	"github.com/pg-sage/sidecar/internal/cases"
	"github.com/pg-sage/sidecar/internal/executor"
)

// The internal tests of package cases classify SQL exactly as production
// does: with the executor's classifier.
func init() { cases.SetTestSQLClassifier(executor.ClassifySQL) }
