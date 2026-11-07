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

// BuildDeleteSQL generates a DELETE for hint_plan.hints scoped to
// pg_sage's own hint text (tuning-02a): the apply INSERT is a no-op when
// any row exists for the query, so a rollback or removal must never delete
// a row whose text pg_sage did not install, an operator's own included.
// See BuildInsertSQL for the dollar-quoting safety rationale; NUL bytes are
// stripped the same way so the DELETE matches the text the INSERT wrote.
func BuildDeleteSQL(queryID int64, hint string) string {
	hint = stripNULs(hint)
	tag := chooseDollarTag(hint)
	return fmt.Sprintf(
		"DELETE FROM hint_plan.hints "+
			"WHERE query_id = %d "+
			"AND application_name = '' "+
			"AND hints = %s%s%s",
		queryID, tag, hint, tag,
	)
}

// InsertedHintText returns the hint text an INSERT built by BuildInsertSQL
// installs for queryID, or false when sql is not such an INSERT.
func InsertedHintText(sql string, queryID int64) (string, bool) {
	prefix := fmt.Sprintf("INSERT INTO hint_plan.hints "+
		"(query_id, application_name, hints) SELECT %d, '', ", queryID)
	i := strings.Index(sql, prefix)
	if i < 0 {
		return "", false
	}
	rest := sql[i+len(prefix):]
	if !strings.HasPrefix(rest, "$") {
		return "", false
	}
	end := strings.IndexByte(rest[1:], '$')
	if end < 0 {
		return "", false
	}
	tag := rest[:end+2]
	body := rest[len(tag):]
	// chooseDollarTag guarantees the tag does not occur in the hint, so its
	// first occurrence closes the literal.
	stop := strings.Index(body, tag)
	if stop < 0 {
		return "", false
	}
	return body[:stop], true
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
