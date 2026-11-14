package plan

import (
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/sqlast"
)

// maxIdentBytes is NAMEDATALEN-1: PostgreSQL silently truncates longer
// names, so a derived name past it would not be the name we planned.
const maxIdentBytes = 63

// checkTarget refuses a statement whose parsed target is not the
// table the plan will be written against.
func checkTarget(stmt sqlast.PlannableAlter, facts TableFacts) error {
	wantSchema := facts.Schema
	if wantSchema == "" {
		wantSchema = "public"
	}
	if stmt.Table != facts.Name || (stmt.Schema != "" && stmt.Schema != wantSchema) {
		return fmt.Errorf("migration SQL targets %q but the request table is %s.%s",
			strings.Trim(stmt.Schema+"."+stmt.Table, "."), wantSchema, facts.Name)
	}
	return nil
}

// checkNames refuses any emitted identifier PostgreSQL would truncate.
func checkNames(names ...string) error {
	for _, name := range names {
		if len(name) > maxIdentBytes {
			return fmt.Errorf("name %q is %d bytes; PostgreSQL truncates names "+
				"longer than %d bytes; plan it by hand", name, len(name), maxIdentBytes)
		}
	}
	return nil
}
