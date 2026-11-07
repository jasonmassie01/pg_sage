package main

import (
	"fmt"
	"runtime"
)

// versionLine is the --version output: build metadata, the SQL validation
// layer and the Go release that built the binary (ci-01).
func versionLine() string {
	return fmt.Sprintf("pg_sage %s (commit: %s, built: %s, sql-ast: %s, go: %s)",
		version, commit, date, sqlASTStatus(), runtime.Version())
}
