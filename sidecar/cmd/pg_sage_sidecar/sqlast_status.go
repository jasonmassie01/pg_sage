package main

import "github.com/pg-sage/sidecar/internal/executor"

// sqlASTStatus names the executor SQL validation layers in this build.
func sqlASTStatus() string {
	if executor.ASTValidationAvailable() {
		return "libpg_query"
	}
	return "unavailable (built without cgo)"
}
