package rolegrants

import "strings"

// QuoteRole quotes a role name for a GRANT unless it is a plain lower-case
// name, so the statement names exactly that role and not a case-folded one.
func QuoteRole(role string) string {
	if role == "" {
		return `""`
	}
	for i, r := range role {
		plain := r >= 'a' && r <= 'z' || r == '_' || (i > 0 && r >= '0' && r <= '9')
		if !plain {
			return `"` + strings.ReplaceAll(role, `"`, `""`) + `"`
		}
	}
	return role
}
