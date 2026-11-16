package executor

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/sqlident"
)

// Exposure remediation contracts (fleetpol-02b). revoke_public_grant takes
// a user table's privileges away from PUBLIC; its rollback is the exact
// re-GRANT of what was revoked. enable_row_level_security turns RLS on and
// always needs an approval (it fails closed for every reader without a
// policy). Both act only on a table an open exposure finding names, and
// only when the policy document's exposure mode is enforce.
const (
	ActionTypeRevokePublicGrant = policy.ActionRevokePublicGrant
	ActionTypeEnableRLS         = policy.ActionEnableRowLevelSecurity
)

var (
	// ErrNotExposureTarget refuses a statement on a table no open exposure
	// finding names, or on a system relation, view or non-table.
	ErrNotExposureTarget = errors.New("not an exposure finding target")
	// ErrExposureNotOwner is permanent: pg_sage's action role neither owns
	// the table nor is a member of its owner, so the revoke cannot take.
	ErrExposureNotOwner = errors.New(
		"pg_sage cannot revoke: grant ownership or run as the owner")
	// ErrExposureNoChange: PUBLIC holds none of the privileges (or RLS is
	// already on); nothing runs and no action is recorded.
	ErrExposureNoChange = errors.New("no_change: nothing to remediate")
	// errExposureTypedOnly refuses an exposure statement reaching the
	// generic recommendation path: only the exposure producer computes its
	// exact rollback.
	errExposureTypedOnly = errors.New("exposure remediations run only through the " +
		"exposure producer")
)

func revokePublicGrantContract() ActionContract {
	return ActionContract{
		ActionType:      ActionTypeRevokePublicGrant,
		BaseRiskTier:    "moderate",
		ProviderSupport: lakebaseOwnedActionProviders(),
		RequiredPermissions: []string{"ownership of the table, or membership in its " +
			"owner role"},
		Prechecks: []string{"exposure.mode is enforce",
			"an open AP-15 finding names the table",
			"the target is a user table (relkind r or p), not a system relation",
			"PUBLIC holds at least one listed privilege (aclexplode, acldefault)"},
		Guardrails: []string{"one transaction", "lock_timeout 2s",
			"revokes only what PUBLIC holds", "change lease on the table"},
		ExecutionPlan: []string{"read relacl with aclexplode (acldefault when NULL)",
			"REVOKE <privileges> ON TABLE s.t FROM PUBLIC",
			"store the exact re-GRANT as rollback_sql and the prior ACL in " +
				"sage.exposure_remediations"},
		SuccessCriteria: []string{"PUBLIC holds none of the revoked privileges",
			"no statement that ran in the hour before stops in the window after"},
		PostChecks: []string{"aclexplode shows no PUBLIC grant of the revoked privileges",
			"reader verification over pg_stat_statements; a stopped reader reverts"},
		RollbackClass: "reversible",
		Cooldown:      "configured cascade cooldown",
		AuditFields:   []string{"object", "prior_acl", "revoked", "finding_id"},
	}
}

func enableRLSContract() ActionContract {
	return ActionContract{
		ActionType:          ActionTypeEnableRLS,
		BaseRiskTier:        "high",
		ProviderSupport:     lakebaseOwnedActionProviders(),
		RequiredPermissions: []string{"ownership of the table"},
		Prechecks: []string{"exposure.mode is enforce",
			"an open AP-03 finding names the table",
			"the target is a user table (relkind r or p)", "RLS is not already enabled"},
		// "approval required" is the gate's guardrail: approval at every
		// trust level, the highest included (spec section 3: fails closed).
		Guardrails: []string{"approval required", "one transaction",
			"lock_timeout 2s", "change lease on the table"},
		ExecutionPlan:   []string{"ALTER TABLE s.t ENABLE ROW LEVEL SECURITY"},
		SuccessCriteria: []string{"relrowsecurity is true"},
		PostChecks: []string{"pg_class.relrowsecurity reads true",
			"the approval card listed the affected statements and roles"},
		RollbackClass: "reversible",
		Cooldown:      "configured cascade cooldown",
		AuditFields:   []string{"object", "roles_losing_access", "finding_id"},
	}
}

type exposureKind int

const (
	exposureNone exposureKind = iota
	exposureRevoke
	exposureRegrant
	exposureEnableRLS
	exposureDisableRLS
)

// exposureStmt is one parsed exposure statement on schema.table.
type exposureStmt struct {
	kind          exposureKind
	schema, table string
	privileges    []string // sorted; revoke and re-grant only
}

func (s exposureStmt) qualified() string { return sqlident.Qualified(s.schema, s.table) }

func (s exposureStmt) actionType() string {
	switch s.kind {
	case exposureRevoke, exposureRegrant:
		return ActionTypeRevokePublicGrant
	case exposureEnableRLS, exposureDisableRLS:
		return ActionTypeEnableRLS
	default:
		return ""
	}
}

// inverse reports the rollback forms, which never run forward.
func (s exposureStmt) inverse() bool {
	return s.kind == exposureRegrant || s.kind == exposureDisableRLS
}

// tablePrivileges are the table privileges REVOKE ALL takes away.
var tablePrivileges = []string{"DELETE", "INSERT", "REFERENCES", "SELECT", "TRIGGER",
	"TRUNCATE", "UPDATE"}

const (
	exposureIdent = `"(?:[^"]|"")+"|[A-Za-z_][A-Za-z0-9_$]*`
	exposurePriv  = `SELECT|INSERT|UPDATE|DELETE|TRUNCATE|REFERENCES|TRIGGER`
	exposureTable = `(` + exposureIdent + `)\s*\.\s*(` + exposureIdent + `)`
	exposurePrivs = `(ALL(?:\s+PRIVILEGES)?|(?:` + exposurePriv + `)(?:\s*,\s*(?:` +
		exposurePriv + `))*)`
)

var (
	revokePublicRE = regexp.MustCompile(`(?i)^\s*REVOKE\s+` + exposurePrivs +
		`\s+ON\s+(?:TABLE\s+)?` + exposureTable + `\s+FROM\s+PUBLIC\s*;?\s*$`)
	regrantPublicRE = regexp.MustCompile(`(?i)^\s*GRANT\s+` + exposurePrivs +
		`\s+ON\s+(?:TABLE\s+)?` + exposureTable + `\s+TO\s+PUBLIC\s*;?\s*$`)
	rowSecurityRE = regexp.MustCompile(`(?i)^\s*ALTER\s+TABLE\s+` + exposureTable +
		`\s+(ENABLE|DISABLE)\s+ROW\s+LEVEL\s+SECURITY\s*;?\s*$`)
)

// parseExposureSQL parses the four exposure forms: REVOKE ... FROM PUBLIC,
// its re-GRANT ... TO PUBLIC, and ALTER TABLE ... ENABLE or DISABLE ROW
// LEVEL SECURITY, each on one schema-qualified table.
func parseExposureSQL(sql string) (exposureStmt, bool) {
	for kind, re := range map[exposureKind]*regexp.Regexp{exposureRevoke: revokePublicRE,
		exposureRegrant: regrantPublicRE} {
		if m := re.FindStringSubmatch(sql); m != nil {
			return exposureStmt{kind: kind, schema: unquoteIdent(m[2]),
				table: unquoteIdent(m[3]), privileges: parsePrivileges(m[1])}, true
		}
	}
	if m := rowSecurityRE.FindStringSubmatch(sql); m != nil {
		kind := exposureEnableRLS
		if strings.EqualFold(m[3], "DISABLE") {
			kind = exposureDisableRLS
		}
		return exposureStmt{kind: kind, schema: unquoteIdent(m[1]),
			table: unquoteIdent(m[2])}, true
	}
	return exposureStmt{}, false
}

func parsePrivileges(list string) []string {
	if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(list)), "ALL") {
		return slices.Clone(tablePrivileges)
	}
	var out []string
	for _, p := range strings.Split(list, ",") {
		out = append(out, strings.ToUpper(strings.TrimSpace(p)))
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// unquoteIdent is the catalog name of an identifier: quoted keeps its
// case, bare folds to lower case.
func unquoteIdent(raw string) string {
	if len(raw) >= 2 && raw[0] == '"' {
		return strings.ReplaceAll(raw[1:len(raw)-1], `""`, `"`)
	}
	return strings.ToLower(raw)
}

// exposureActionType is the contract an exposure statement runs under, or "".
func exposureActionType(sql string) string {
	stmt, ok := parseExposureSQL(sql)
	if !ok {
		return ""
	}
	return stmt.actionType()
}

// checkExposureSQL is the validator's rule for REVOKE, GRANT and the RLS
// forms of ALTER TABLE: exactly a parsed exposure statement on a table
// outside the protected schemas.
func checkExposureSQL(sql string) error {
	stmt, ok := parseExposureSQL(sql)
	if !ok {
		return fmt.Errorf("%w: only REVOKE <privileges> ON TABLE s.t FROM PUBLIC, its "+
			"re-GRANT TO PUBLIC and ALTER TABLE s.t ENABLE|DISABLE ROW LEVEL SECURITY "+
			"are allowed", ErrDisallowedSQL)
	}
	if isProtectedExecutorSchema(stmt.schema) || strings.HasPrefix(stmt.schema, "pg_") {
		return fmt.Errorf("%w: %s is a protected schema", ErrDisallowedSQL, stmt.schema)
	}
	return nil
}

// isExposureInverseSQL is a re-grant or RLS disable: rollback-only.
func isExposureInverseSQL(sql string) bool {
	stmt, ok := parseExposureSQL(sql)
	return ok && stmt.inverse()
}

// exposureRollbackContract classifies an exposure rollback: the inverse of
// the forward statement on the same table, under the forward's contract.
func exposureRollbackContract(forwardSQL, rollbackSQL string) (ActionContract, bool) {
	forward, ok := parseExposureSQL(forwardSQL)
	if !ok || forward.inverse() {
		return ActionContract{}, false
	}
	back, ok := parseExposureSQL(rollbackSQL)
	if !ok || !back.inverse() || back.actionType() != forward.actionType() ||
		back.schema != forward.schema || back.table != forward.table {
		return ActionContract{}, false
	}
	return ContractForActionType(forward.actionType())
}

func revokeSQL(schema, table string, privileges []string) string {
	return "REVOKE " + strings.Join(privileges, ", ") + " ON TABLE " +
		sqlident.Qualified(schema, table) + " FROM PUBLIC"
}

func regrantSQL(schema, table string, privileges []string) string {
	return "GRANT " + strings.Join(privileges, ", ") + " ON TABLE " +
		sqlident.Qualified(schema, table) + " TO PUBLIC"
}

func rowSecuritySQL(schema, table, verb string) string {
	return "ALTER TABLE " + sqlident.Qualified(schema, table) + " " + verb +
		" ROW LEVEL SECURITY"
}

// revokablePrivileges is requested ∩ held, sorted: what a revoke takes.
func revokablePrivileges(requested, held []string) []string {
	var out []string
	for _, p := range requested {
		if slices.Contains(held, p) && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	slices.Sort(out)
	return out
}
