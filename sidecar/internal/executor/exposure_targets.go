package executor

import (
	"fmt"
	"slices"
	"strings"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/sqlident"
	"github.com/pg-sage/sidecar/internal/workload"
)

// Exposure findings (agentposture, fleetpol-02a) and the remediations they
// name. AP-15 is one finding per schema whose evidence lists each relation
// granted to PUBLIC; AP-03 is one finding per exposed table.
const (
	exposureCategoryPublicGrants = "agent_posture:AP-15"
	exposureCategoryRLS          = "agent_posture:AP-03"
	exposureEvidenceSource       = "pg_class.relacl"
	// rlsCardStatements is how many statements the RLS approval card lists.
	rlsCardStatements = 20
)

// exposureStatement is one pg_stat_statements entry that references an
// exposed table: its fingerprint and calls, never its text.
type exposureStatement struct {
	QueryID int64 `json:"queryid"`
	Calls   int64 `json:"calls"`
}

// exposureTargetRefusal is nil when f is an open exposure finding of the
// statement's kind that names its table, which is not in a system schema.
func exposureTargetRefusal(f analyzer.Finding, stmt exposureStmt) error {
	refuse := func(why string) error {
		return fmt.Errorf("%w: %s (%s)", ErrNotExposureTarget, stmt.qualified(), why)
	}
	switch {
	case stmt.inverse():
		return refuse("a re-grant or RLS disable is a rollback, not a remediation")
	case isProtectedExecutorSchema(stmt.schema) || strings.HasPrefix(stmt.schema, "pg_"):
		return refuse("system schema")
	case stmt.kind == exposureRevoke && f.Category != exposureCategoryPublicGrants,
		stmt.kind == exposureEnableRLS && f.Category != exposureCategoryRLS:
		return refuse("finding " + f.Category + " does not call for " + stmt.actionType())
	}
	for _, target := range findingExposureTargets(f) {
		if target == [2]string{stmt.schema, stmt.table} {
			return nil
		}
	}
	return refuse("the finding does not name the table")
}

// findingExposureTargets are the (schema, table) pairs f names: an AP-03
// table finding its object, an AP-15 schema finding each relation of its
// evidence, and one of its remediations (object type table) its table when
// the evidence lists it.
func findingExposureTargets(f analyzer.Finding) [][2]string {
	switch {
	case f.Category == exposureCategoryRLS && f.ObjectType == "table":
		if schema, table, ok := splitQualified(f.ObjectIdentifier); ok {
			return [][2]string{{schema, table}}
		}
	case f.Category == exposureCategoryPublicGrants && f.ObjectType == "schema":
		return evidenceTargets(f.Detail)
	case f.Category == exposureCategoryPublicGrants && f.ObjectType == "table":
		schema, table, ok := splitQualified(f.ObjectIdentifier)
		if ok && slices.Contains(evidenceTargets(f.Detail), [2]string{schema, table}) {
			return [][2]string{{schema, table}}
		}
	}
	return nil
}

func evidenceTargets(detail map[string]any) [][2]string {
	var out [][2]string
	for _, ref := range evidenceRefs(detail) {
		if schema, table, ok := splitQualified(ref); ok {
			out = append(out, [2]string{schema, table})
		}
	}
	return out
}

// evidenceRefs are the relation refs of a finding detail's evidence list,
// as stored (jsonb) or as built ([]Evidence-shaped maps).
func evidenceRefs(detail map[string]any) []string {
	items, _ := detail["evidence"].([]any)
	var out []string
	for _, item := range items {
		m, _ := item.(map[string]any)
		source, _ := m["source"].(string)
		ref, _ := m["ref"].(string)
		if source == exposureEvidenceSource && ref != "" {
			out = append(out, ref)
		}
	}
	return out
}

// splitQualified parses a schema-qualified name as sqlident renders it.
func splitQualified(name string) (string, string, bool) {
	schema, table, next, err := workload.ReadQualified(name, 0)
	if err != nil || schema == "" || table == "" || strings.TrimSpace(name[next:]) != "" {
		return "", "", false
	}
	return schema, table, true
}

// exposureRemediations are the typed proposals an open exposure finding
// yields: a REVOKE ALL ... FROM PUBLIC per AP-15 relation (narrowed to
// what PUBLIC holds when it runs; its re-GRANT is computed then) and an
// ENABLE ROW LEVEL SECURITY with its DISABLE for an AP-03 table.
func exposureRemediations(f analyzer.Finding) []analyzer.Finding {
	var out []analyzer.Finding
	for _, target := range findingExposureTargets(f) {
		r := f
		r.ObjectType = "table"
		r.ObjectIdentifier = sqlident.Qualified(target[0], target[1])
		if f.Category == exposureCategoryRLS {
			r.RecommendedSQL = rowSecuritySQL(target[0], target[1], "ENABLE")
			r.RollbackSQL = rowSecuritySQL(target[0], target[1], "DISABLE")
			r.Title = "Enable row-level security on " + r.ObjectIdentifier
		} else {
			r.RecommendedSQL = "REVOKE ALL ON TABLE " + r.ObjectIdentifier + " FROM PUBLIC"
			r.Title = "Revoke PUBLIC grants on " + r.ObjectIdentifier
		}
		out = append(out, r)
	}
	return out
}

// rlsApprovalCard is what the approval of an RLS enable shows: the top
// statements by calls that reference the table (fingerprints only), the
// roles that lose access, and a CREATE POLICY template that is card text,
// never executed.
func rlsApprovalCard(schema, table string, stmts []exposureStatement,
	roles []string) map[string]any {
	listed := slices.Clone(stmts)
	slices.SortStableFunc(listed, func(a, b exposureStatement) int {
		return int(b.Calls - a.Calls)
	})
	if len(listed) > rlsCardStatements {
		listed = listed[:rlsCardStatements]
	}
	name := sqlident.Qualified(schema, table)
	return map[string]any{
		"object": name, "affected_statements": listed,
		"roles_losing_access": slices.Clone(roles),
		"policy_template": "-- Optional, review before use: with RLS on and no " +
			"policy, only the owner and BYPASSRLS roles read " + name + ".\n" +
			"CREATE POLICY " + sqlident.Ident(table+"_read") + " ON " + name +
			" FOR SELECT TO <role> USING (<predicate>);",
		"policy_template_executed": false,
	}
}
