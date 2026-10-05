package tuner

import (
	"regexp"
	"strings"
)

// validHintTokens are the allowed pg_hint_plan directive prefixes.
var validHintTokens = []string{
	"Set(", "HashJoin(", "MergeJoin(", "NestLoop(",
	"IndexScan(", "IndexOnlyScan(", "SeqScan(", "NoSeqScan(",
	"Parallel(", "NoParallel(",
	"BitmapScan(", "NoBitmapScan(",
	"NoIndexScan(", "NoNestLoop(", "NoHashJoin(", "NoMergeJoin(",
}

// dangerousPatterns rejects SQL injection attempts.
var dangerousPatterns = regexp.MustCompile(
	`(?i)(;|--|\b(DROP|DELETE|INSERT|ALTER|CREATE|TRUNCATE|UPDATE|GRANT|REVOKE)\b)`,
)

// validateHintSyntax checks a hint string contains only valid
// pg_hint_plan tokens and no dangerous SQL.
func validateHintSyntax(hint string) bool {
	hint = strings.TrimSpace(hint)
	if hint == "" {
		return false
	}
	if dangerousPatterns.MatchString(hint) {
		return false
	}
	// Every non-whitespace token must start with a known prefix.
	// Split on ")  " boundaries to isolate directives.
	parts := splitHintDirectives(hint)
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if !hasValidPrefix(p) {
			return false
		}
	}
	return true
}

func hasValidPrefix(s string) bool {
	for _, tok := range validHintTokens {
		if strings.HasPrefix(s, tok) {
			return true
		}
	}
	return false
}

// splitHintDirectives splits a combined hint string like
// "Set(work_mem \"256MB\") HashJoin(t1 t2)" into individual
// directive strings.
func splitHintDirectives(hint string) []string {
	var parts []string
	depth := 0
	start := 0
	for i, ch := range hint {
		switch ch {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				parts = append(parts, hint[start:i+1])
				start = i + 1
			}
		}
	}
	// Trailing text (shouldn't happen in valid hints)
	if trail := strings.TrimSpace(hint[start:]); trail != "" {
		parts = append(parts, trail)
	}
	return parts
}
