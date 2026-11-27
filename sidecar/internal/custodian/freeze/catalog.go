package freeze

import (
	"fmt"
	"strings"
)

// ResponsePlanCatalogFreeze is the operator plan for red system catalogs:
// a catalog VACUUM needs the MAINTAIN privilege or ownership, which a
// managed provider rarely grants, so pg_sage never runs it.
const ResponsePlanCatalogFreeze ResponseKind = "plan_catalog_freeze"

// VacuumFreezeSQL is the one VACUUM (FREEZE) statement pg_sage builds for
// a table (no trailing semicolon, so dedup and backoff keys agree).
func VacuumFreezeSQL(schema, table string) string {
	return "VACUUM (FREEZE) " + quoteIdent(schema) + "." + quoteIdent(table)
}

// CatalogTarget is one red system catalog.
type CatalogTarget struct {
	Schema, Table string
	XIDAge        int64
	MaxAge        int64
}

// CatalogFreezePlan answers the red catalogs of one scan with a single
// plan and no SQL; it errs when there is nothing to plan.
func CatalogFreezePlan(targets []CatalogTarget) (Response, error) {
	if len(targets) == 0 {
		return Response{}, fmt.Errorf("catalog freeze plan needs at least one catalog")
	}
	statements := make([]string, 0, len(targets))
	tables := make([]map[string]any, 0, len(targets))
	for _, t := range targets {
		if strings.TrimSpace(t.Schema) == "" || strings.TrimSpace(t.Table) == "" {
			return Response{}, fmt.Errorf("schema-qualified catalog target is required")
		}
		statements = append(statements, VacuumFreezeSQL(t.Schema, t.Table)+";")
		tables = append(tables, map[string]any{"table": t.Schema + "." + t.Table,
			"xid_age": t.XIDAge, "freeze_max_age": t.MaxAge})
	}
	plan := "System catalogs are near their freeze maximum. As a role with MAINTAIN on " +
		"them (or their owner) run: " + strings.Join(statements, " ") +
		" Otherwise make sure autovacuum's anti-wraparound VACUUM is not held back " +
		"by an xmin holder."
	return Response{Kind: ResponsePlanCatalogFreeze, Plan: plan,
		Evidence: map[string]any{"catalogs": tables, "plan_only": true}}, nil
}
