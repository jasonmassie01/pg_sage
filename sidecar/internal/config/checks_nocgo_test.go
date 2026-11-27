//go:build !cgo

package config

import (
	"errors"
	"testing"

	"github.com/pg-sage/sidecar/internal/sqlast"
)

// A build without the SQL parser must refuse every owner check at load:
// it never runs SQL it did not parse (market-11).
func TestChecks_NoCgoBuildRefusesChecksAtLoad(t *testing.T) {
	_, err := PrepareCheck(validCheck())
	if !errors.Is(err, sqlast.ErrUnavailable) {
		t.Fatalf("PrepareCheck = %v, want ErrUnavailable", err)
	}
	_, err = loadYAMLResult(t, standaloneIOYAML+`
checks:
  - id: no_pk
    title: No primary key
    severity: warning
    sql: SELECT relname FROM pg_class
    object_column: relname
`)
	if !errors.Is(err, sqlast.ErrUnavailable) {
		t.Fatalf("Load = %v, want ErrUnavailable", err)
	}
}
