package main

import (
	"os"
	"testing"

	"github.com/pg-sage/sidecar/internal/testdb"
)

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "cmd_pg_sage_sidecar"))
}
