package cases

import "strings"

func IdentityKeyForFinding(f SourceFinding) string {
	objectKey := f.ObjectIdentifier
	if f.ObjectType == "query" {
		objectKey = normalizedQuery(f)
	}

	parts := []string{
		"finding",
		f.DatabaseName,
		f.Category,
		f.ObjectType,
		objectKey,
	}
	if f.RuleID != "" {
		parts = append(parts, f.RuleID)
	}

	return strings.Join(parts, ":")
}

func normalizedQuery(f SourceFinding) string {
	if f.Detail == nil {
		return f.ObjectIdentifier
	}

	value, ok := f.Detail["normalized_query"].(string)
	if !ok || value == "" {
		return f.ObjectIdentifier
	}
	return value
}
