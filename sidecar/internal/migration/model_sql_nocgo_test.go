//go:build !cgo

package migration

import (
	"errors"
	"testing"

	"github.com/pg-sage/sidecar/internal/sqlast"
)

// Without the parser no model script can be validated, however safe.
func TestValidateModelDDLRefusesEverythingWithoutCgo(t *testing.T) {
	err := validateModelDDL("CREATE INDEX CONCURRENTLY i ON public.orders (x)",
		modelTarget{Table: "orders"})
	if !errors.Is(err, sqlast.ErrUnavailable) {
		t.Fatalf("err = %v, want sqlast.ErrUnavailable", err)
	}
}
