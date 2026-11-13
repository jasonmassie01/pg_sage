package main

import "github.com/pg-sage/sidecar/internal/executor"

// sqlASTAssumeTag is the test-only build tag of sqlast_assumed_e2e.go.
const sqlASTAssumeTag = "sage_e2e_assume_sqlast"

// sqlASTStatus names the executor SQL validation layers in this build.
func sqlASTStatus() string {
	return sqlASTStatusFor(executor.ASTValidationAvailable(), sqlASTAssumed)
}

// sqlASTStatusFor names the validation layer: an e2e test build that only
// assumes the parse-tree layer says so before anything else.
func sqlASTStatusFor(available, assumed bool) string {
	switch {
	case assumed:
		return "assumed (e2e test build " + sqlASTAssumeTag + "; never run in production)"
	case available:
		return "libpg_query"
	}
	return "unavailable (built without cgo)"
}
