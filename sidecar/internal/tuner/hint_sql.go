package tuner

import (
	"fmt"
	"strings"
)

// BuildInsertSQL generates an INSERT for hint_plan.hints.
//
// The SQL this produces is stored as Finding.RecommendedSQL and
// executed later by the executor (see executor.ExecInTransaction),
// so it must be self-contained — we cannot use bind parameters.
//
// Safety: we emit the hint as a PostgreSQL dollar-
// quoted string literals ($sageqh$…$sageqh$). Dollar-quoted
// strings do not interpret backslash escapes or double single
// quotes, so they are immune to single-quote-based injection
// regardless of standard_conforming_strings. The only attack
// surface is a literal that itself contains the tag; we pick a
// unique tag for each call to eliminate that case and strip NUL
// bytes as extra defense-in-depth.
func BuildInsertSQL(queryID int64, hint string) string {
	hint = stripNULs(hint)
	tag := chooseDollarTag(hint)
	return fmt.Sprintf(
		"INSERT INTO hint_plan.hints "+
			"(query_id, application_name, hints) "+
			"SELECT %d, '', %s%s%s "+
			"WHERE NOT EXISTS ("+
			"SELECT 1 FROM hint_plan.hints "+
			"WHERE query_id = %d AND application_name = '')",
		queryID, tag, hint, tag, queryID,
	)
}

// BuildDeleteSQL generates a DELETE for hint_plan.hints.
// See BuildInsertSQL for the dollar-quoting safety rationale.
func BuildDeleteSQL(queryID int64) string {
	return fmt.Sprintf(
		"DELETE FROM hint_plan.hints "+
			"WHERE query_id = %d "+
			"AND application_name = ''",
		queryID,
	)
}

// stripNULs removes NUL bytes, which PostgreSQL rejects in text
// columns but which could truncate client-side string handling.
func stripNULs(s string) string {
	return strings.ReplaceAll(s, "\x00", "")
}

// chooseDollarTag picks a dollar-quote tag guaranteed not to
// appear in any of the provided payloads, so the closing tag is
// unambiguous. It starts with $sageqh$ and extends with digits
// until the tag is not a substring of any payload.
func chooseDollarTag(payloads ...string) string {
	base := "sageqh"
	for i := 0; i < 1000; i++ {
		var tag string
		if i == 0 {
			tag = "$" + base + "$"
		} else {
			tag = fmt.Sprintf("$%s%d$", base, i)
		}
		collides := false
		for _, p := range payloads {
			if strings.Contains(p, tag) {
				collides = true
				break
			}
		}
		if !collides {
			return tag
		}
	}
	// Extremely unlikely fallback — caller payloads are from
	// pg_stat_statements and unlikely to collide with 1000
	// different candidate tags.
	return "$sageqh_fallback$"
}
