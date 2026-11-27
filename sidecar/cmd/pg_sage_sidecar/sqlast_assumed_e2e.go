//go:build sage_e2e_assume_sqlast

package main

import "github.com/pg-sage/sidecar/internal/executor"

// sqlASTAssumed: this binary was built for the full-binary e2e suite on a
// host without a C compiler (executor-10). Its gate treats the parse-tree
// SQL layer as present, as the in-process tests do, so the suite can watch
// unattended changes run; the parse-tree check itself stays a no-op. No
// release, image or CI recipe builds with this tag
// (TestNoBuildRecipeUsesTheSQLASTAssumptionTag), and startup warns.
const sqlASTAssumed = true

func init() { executor.AssumeASTValidationForTests() }
