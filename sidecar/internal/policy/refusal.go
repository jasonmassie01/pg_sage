package policy

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Refusal tokens accepted in a policy document's refusal_set. Each is a
// precise predicate over the request, not a ban on a whole action family
// (D1): unused, duplicate and invalid index drops are derivable and keep
// their earned autonomy; VACUUM and ANALYZE need no rollback and are never
// "unrollbackable".
const (
	// RefusalRLSChange: enabling, disabling or forcing row-level security,
	// or creating, altering or dropping a row-security policy.
	RefusalRLSChange = "rls_change"
	// RefusalGrantExpansion: GRANT (object or role), ALTER ROLE/USER/GROUP,
	// OWNER TO and ALTER DEFAULT PRIVILEGES.
	RefusalGrantExpansion = "grant_expansion"
	// RefusalMajorUpgrade: major-version or extension upgrades.
	RefusalMajorUpgrade = "major_upgrade"
	// RefusalNonDupObjectDrop: dropping an object a definition cannot
	// restore (table, column, constraint, sequence, schema, replication
	// slot). Index drops are derivable and are not refused.
	RefusalNonDupObjectDrop = "non_dup_object_drop"
	// RefusalUnrollbackable: rollback class not_reversible or
	// forward_fix_only, unless the request runs under an owner declaration.
	RefusalUnrollbackable = "unrollbackable"
)

var (
	rlsChangeSQL = regexp.MustCompile(`(?i)\b(ENABLE|DISABLE|FORCE)\s+ROW\s+LEVEL\s+` +
		`SECURITY\b|\b(CREATE|ALTER|DROP)\s+POLICY\b`)
	grantExpansionSQL = regexp.MustCompile(`(?i)^\s*GRANT\b|\bALTER\s+(ROLE|USER|GROUP)\b|` +
		`\bOWNER\s+TO\b|\bALTER\s+DEFAULT\s+PRIVILEGES\b`)
	extensionUpgradeSQL = regexp.MustCompile(`(?is)^\s*ALTER\s+EXTENSION\b.*\bUPDATE\b`)
	nonDerivableDropSQL = regexp.MustCompile(`(?i)^\s*DROP\s+(TABLE|SCHEMA|SEQUENCE|VIEW|` +
		`MATERIALIZED\s+VIEW|TYPE|DOMAIN|FUNCTION|PROCEDURE|EXTENSION|DATABASE|` +
		`PUBLICATION|SUBSCRIPTION)\b|\bpg_drop_replication_slot\s*\(`)
	alterTableSQL      = regexp.MustCompile(`(?i)^\s*ALTER\s+TABLE\b`)
	alterTableDropWord = regexp.MustCompile(`(?i)\bDROP\s+(\w+)`)
)

var refusalPredicates = map[string]func(ActionRequest) bool{
	RefusalRLSChange:        rlsChange,
	RefusalGrantExpansion:   grantExpansion,
	RefusalMajorUpgrade:     majorUpgrade,
	RefusalNonDupObjectDrop: nonDerivableDrop,
	RefusalUnrollbackable:   unrollbackable,
}

// KnownRefusalTokens lists the tokens a refusal_set may contain.
func KnownRefusalTokens() []string {
	tokens := make([]string, 0, len(refusalPredicates))
	for token := range refusalPredicates {
		tokens = append(tokens, token)
	}
	sort.Strings(tokens)
	return tokens
}

// RefusedBy reports the first token of doc.RefusalSet, in document order,
// whose predicate matches req.
func RefusedBy(doc Document, req ActionRequest) (string, bool) {
	if req.Contract == nil {
		return "", false
	}
	for _, token := range doc.RefusalSet {
		if predicate, ok := refusalPredicates[token]; ok && predicate(req) {
			return token, true
		}
	}
	return "", false
}

// refusalDecision sends a refused self-initiated request to a human. It is
// applied only to an otherwise-execute verdict, so it can never widen a
// verdict; read-only diagnostics mutate nothing and are never refused.
func refusalDecision(doc Document, req ActionRequest) (Decision, bool) {
	if req.Contract.RiskTier == RiskReadOnly {
		return Decision{}, false
	}
	token, refused := RefusedBy(doc, req)
	if !refused {
		return Decision{}, false
	}
	decision := decisionForRequest(req, blockedAs(VerdictQueueApproval, ReasonRefusedByPolicy))
	decision.Detail = token
	return decision, true
}

func validateRefusalSet(tokens []string) error {
	for _, token := range tokens {
		if _, ok := refusalPredicates[token]; !ok {
			return fmt.Errorf("refusal_set: unknown token %q (known: %s)",
				token, strings.Join(KnownRefusalTokens(), ", "))
		}
	}
	return nil
}

func rlsChange(req ActionRequest) bool { return rlsChangeSQL.MatchString(req.SQL) }

func grantExpansion(req ActionRequest) bool { return grantExpansionSQL.MatchString(req.SQL) }

func nonDerivableDrop(req ActionRequest) bool { return dropKind(req) == DropNonDerivable }

func majorUpgrade(req ActionRequest) bool {
	switch req.Contract.ActionType {
	case "major_upgrade", "extension_update":
		return true
	}
	return extensionUpgradeSQL.MatchString(req.SQL)
}

func unrollbackable(req ActionRequest) bool {
	class := req.Contract.RollbackClass
	return (class == RollbackNotReversible || class == RollbackForwardFixOnly) &&
		!req.OwnerDeclared
}

// dropKind is the contract's declared drop kind, or the kind derived from
// the SQL when the contract declares none.
func dropKind(req ActionRequest) DropKind {
	if req.Contract.DropKind != "" {
		return req.Contract.DropKind
	}
	if nonDerivableDropSQL.MatchString(req.SQL) || alterTableDropsObject(req.SQL) {
		return DropNonDerivable
	}
	return ""
}

// alterTableDropsObject reports ALTER TABLE ... DROP [COLUMN|CONSTRAINT]
// ... . ALTER COLUMN ... DROP DEFAULT / NOT NULL / EXPRESSION / IDENTITY
// change a column and drop nothing.
func alterTableDropsObject(sql string) bool {
	if !alterTableSQL.MatchString(sql) {
		return false
	}
	for _, match := range alterTableDropWord.FindAllStringSubmatch(sql, -1) {
		switch strings.ToUpper(match[1]) {
		case "DEFAULT", "NOT", "EXPRESSION", "IDENTITY":
			continue
		}
		return true
	}
	return false
}

func knownRollbackClass(class RollbackClass) bool {
	switch class {
	case "", RollbackReversible, RollbackNoRollbackNeeded, RollbackNotApplicable,
		RollbackApplication, RollbackForwardFixOnly, RollbackNotReversible:
		return true
	}
	return false
}

func knownDropKind(kind DropKind) bool {
	return kind == "" || kind == DropDerivable || kind == DropNonDerivable
}
