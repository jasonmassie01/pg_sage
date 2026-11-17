package main

import (
	"os"
	"testing"

	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/testsupport/restorecmd"
)

func TestMain(m *testing.M) {
	// drill-01c: the binary may be started as a fake restore program.
	if restorecmd.Main() {
		return
	}
	// cgo-less local builds would queue every unattended mutation.
	executor.AssumeASTValidationForTests()
	os.Exit(testdb.Run(func() int {
		// A characterization test re-runs this binary as the sidecar.
		if args := os.Getenv(mainChildEnv); args != "" {
			return runMainChild(args)
		}
		return m.Run()
	}, "cmd_pg_sage_sidecar"))
}
