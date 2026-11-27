package cases

import "strings"

// IdentityKeyForFinding is a finding case's identity: database, category,
// object type, object identifier and, when set, rule id.
func IdentityKeyForFinding(f SourceFinding) string {
	parts := []string{
		"finding",
		f.DatabaseName,
		f.Category,
		f.ObjectType,
		f.ObjectIdentifier,
	}
	if f.RuleID != "" {
		parts = append(parts, f.RuleID)
	}

	return strings.Join(parts, ":")
}
