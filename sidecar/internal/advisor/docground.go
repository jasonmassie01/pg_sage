package advisor

import (
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/pgconf"
)

// The documentation prior (A3) and its safe ranges live in pgconf.Docs,
// shared with the executor's allowlists (G-P0-1). The LLM proposes within
// the documented region, and ValidateGUCValue gates the result.

// DocContext returns the documentation prior for the named GUCs, to be
// embedded in an LLM prompt, with the version notes that hold on pgMajor
// (0: unknown, none). Unknown GUCs are skipped; none known gives "".
func DocContext(pgMajor int, gucs ...string) string {
	var b strings.Builder
	for _, g := range gucs {
		doc, ok := pgconf.Docs[g]
		if !ok {
			continue
		}
		fmt.Fprintf(&b, "- %s: %s %s Safe range: %s. ",
			g, doc.Description, doc.Guidance, pgconf.RangeString(doc))
		if doc.BaseUnit != "" {
			fmt.Fprintf(&b, "A value without a unit is in %s. ", doc.BaseUnit)
		}
		for _, note := range doc.NotesFor(pgMajor) {
			fmt.Fprintf(&b, "Version note (PG%d): %s ", pgMajor, note)
		}
		b.WriteString("\n")
	}
	if b.Len() == 0 {
		return ""
	}
	return "PostgreSQL parameter documentation (authoritative — base your " +
		"recommendation on this):\n" + b.String()
}

// ValidateGUCValue reports whether a proposed value is within the
// documented safe range for a known GUC. Unknown GUCs pass (ok=true) —
// the validator only gates parameters it has a documented opinion on;
// whether an unknown GUC may run is applyConfigAllowlist's decision.
// Unitless values are read in the GUC's PostgreSQL base unit.
func ValidateGUCValue(guc, value string) (ok bool, reason string) {
	return pgconf.ValidateValue(guc, value)
}

// ValidateConfigSQL validates the value(s) of an
// "ALTER SYSTEM SET <guc> = <value>" statement or an
// "ALTER TABLE <t> SET (<reloption> = <value>, ...)" statement against
// the documented safe ranges (A3). Other SQL passes through.
func ValidateConfigSQL(sql string) (ok bool, reason string) {
	if stmt, found := pgconf.ParseAlterTableReloptions(sql); found {
		if stmt.Reset {
			return true, ""
		}
		for _, opt := range stmt.Options {
			if ok, reason := pgconf.ValidateReloption(opt.Key, opt.Value); !ok {
				return false, reason
			}
		}
		return true, ""
	}
	stmt, found := pgconf.ParseAlterSystem(sql)
	if !found || stmt.Reset {
		return true, ""
	}
	return ValidateGUCValue(stmt.Name, stmt.Value)
}
