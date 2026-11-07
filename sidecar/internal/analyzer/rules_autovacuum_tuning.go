package analyzer

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/sanitize"
)

// ruleAutovacuumTuning recommends a per-table autovacuum_vacuum_scale_factor
// for large, write-heavy tables. PostgreSQL's global default scale factor
// (0.20) means a 10M-row table accumulates ~2M dead tuples before
// autovacuum runs — far too coarse. The fix is a durable per-table
// reloption and is fully reversible (RESET, or SET back to the table's
// explicit prior value), so it is a SAFE-tier action (A1). It complements
// ruleTableBloat (a one-off VACUUM): this rule fixes the *cause*
// (mis-tuned autovacuum), not the symptom.
func ruleAutovacuumTuning(
	current *collector.Snapshot,
	_ *collector.Snapshot,
	cfg *config.Config,
	_ *RuleExtras,
) []Finding {
	if current == nil {
		return nil
	}
	minRows := int64(cfg.Analyzer.AutovacuumTuneMinRows)
	if minRows <= 0 {
		minRows = 1_000_000
	}

	var findings []Finding
	for _, t := range current.Tables {
		live := t.NLiveTup
		if live < minRows {
			continue // tuning small tables is pointless
		}
		// Write-heavy heuristic: lifetime updates+deletes exceed the
		// current live-row count, i.e. the table churns enough that the
		// default scale factor lets too many dead tuples build up.
		if t.NTupUpd+t.NTupDel < live {
			continue
		}
		// The table's own reloptions, read with its stats every cycle, so a
		// DBA's tuning holds with or without an LLM (rules-02). Unreadable
		// options or autovacuum turned off: the DBA's intent is unknown or
		// deliberate, so leave the table alone.
		opts, ok := parseAutovacuumOptions(t.Reloptions)
		if !ok || opts.disabled {
			continue
		}
		target := targetScaleFactor(live)
		// Already tuned at or below the target: never loosen a DBA's
		// setting, and never re-apply our own forever (G2-B05).
		if opts.hasScaleFactor && opts.scaleFactor <= target {
			continue
		}
		findings = append(findings, autovacuumTuningFinding(t, target, opts))
	}
	return findings
}

// autovacuumTuningFinding builds the SAFE-tier per-table recommendation.
// The rollback restores an explicit prior setting instead of resetting it
// to the global default.
func autovacuumTuningFinding(
	t collector.TableStats, target float64, opts autovacuumOptions,
) Finding {
	ident := t.SchemaName + "." + t.RelName
	q := sanitize.QuoteQualifiedName(t.SchemaName, t.RelName)
	detail := map[string]any{
		"n_live_tup":               t.NLiveTup,
		"n_dead_tup":               t.NDeadTup,
		"lifetime_updates":         t.NTupUpd,
		"lifetime_deletes":         t.NTupDel,
		"recommended_scale_factor": target,
		"default_scale_factor":     0.20,
	}
	rollback := fmt.Sprintf("ALTER TABLE %s RESET (autovacuum_vacuum_scale_factor);", q)
	if opts.hasScaleFactor {
		detail["current_scale_factor"] = opts.scaleFactor
		rollback = fmt.Sprintf("ALTER TABLE %s SET (autovacuum_vacuum_scale_factor = %s);",
			q, autovacuumReloptionLiteral(opts.scaleFactorText))
	}
	return Finding{
		Category:         "autovacuum_tuning",
		Severity:         "warning",
		ObjectType:       "table",
		ObjectIdentifier: ident,
		Title: fmt.Sprintf(
			"Table %s should use autovacuum_vacuum_scale_factor=%.2f",
			ident, target),
		Detail: detail,
		Recommendation: fmt.Sprintf(
			"Set autovacuum_vacuum_scale_factor=%.2f on %s so autovacuum "+
				"triggers before dead tuples accumulate; the default 0.20 "+
				"is too coarse for a table this large and write-heavy. "+
				"Reversible: the rollback restores the table's current setting.",
			target, ident),
		RecommendedSQL: fmt.Sprintf(
			"ALTER TABLE %s SET (autovacuum_vacuum_scale_factor = %.2f);",
			q, target),
		RollbackSQL: rollback,
		ActionRisk:  "safe",
	}
}

// targetScaleFactor scales the recommended autovacuum_vacuum_scale_factor
// down as the table grows, keeping the absolute dead-tuple count that
// triggers autovacuum roughly bounded instead of growing with the table.
func targetScaleFactor(liveRows int64) float64 {
	switch {
	case liveRows >= 50_000_000:
		return 0.01
	case liveRows >= 10_000_000:
		return 0.02
	default:
		return 0.05
	}
}

// autovacuumOptions is what the rule needs from a table's reloptions.
// scaleFactorText is the stored value exactly as pg_class holds it, so a
// rollback restores the DBA's text ("0.10", not "0.1"; " 0.10" quoted).
type autovacuumOptions struct {
	scaleFactor     float64
	scaleFactorText string
	hasScaleFactor  bool
	disabled        bool
}

// plainDecimal is a numeric literal PostgreSQL's lexer keeps as written
// when it becomes a reloption (an integer or float constant, no sign).
var plainDecimal = regexp.MustCompile(`^([0-9]+\.?[0-9]*|\.[0-9]+)([eE][-+]?[0-9]+)?$`)

// zeroPaddedInteger is an integer constant PostgreSQL would store without
// its leading zeros (a bare 007 becomes "7"), so it must stay quoted.
var zeroPaddedInteger = regexp.MustCompile(`^0[0-9]+$`)

// autovacuumReloptionLiteral renders a stored scale factor for ALTER TABLE
// SET: bare when it is a plain decimal, otherwise as a string literal,
// which PostgreSQL stores verbatim after validating it.
func autovacuumReloptionLiteral(value string) string {
	if plainDecimal.MatchString(value) && !zeroPaddedInteger.MatchString(value) {
		return value
	}
	return sanitize.QuoteLiteral(value)
}

// parseAutovacuumOptions reads pg_class.reloptions entries such as
// "autovacuum_vacuum_scale_factor=0.05". ok is false when an entry is
// not name=value, the scale factor is not a finite number >= 0 (PostgreSQL
// allows whitespace around it, as parse_real does), or
// autovacuum_enabled is not a PostgreSQL boolean.
func parseAutovacuumOptions(reloptions []string) (autovacuumOptions, bool) {
	var out autovacuumOptions
	for _, opt := range reloptions {
		name, value, found := strings.Cut(opt, "=")
		if !found || name == "" {
			return autovacuumOptions{}, false
		}
		switch name {
		case "autovacuum_vacuum_scale_factor":
			sf, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
			if err != nil || math.IsNaN(sf) || math.IsInf(sf, 0) || sf < 0 {
				return autovacuumOptions{}, false
			}
			out.scaleFactor, out.scaleFactorText, out.hasScaleFactor = sf, value, true
		case "autovacuum_enabled":
			enabled, valid := parsePGBool(value)
			if !valid {
				return autovacuumOptions{}, false
			}
			out.disabled = !enabled
		}
	}
	return out, true
}

// parsePGBool accepts what PostgreSQL's parse_bool accepts: any
// case-insensitive prefix of true, false, yes or no; on; "of" or "off"
// ("o" alone is ambiguous); 1 and 0.
func parsePGBool(value string) (bool, bool) {
	v := strings.ToLower(value)
	switch {
	case v == "":
		return false, false
	case v == "on" || v == "1":
		return true, true
	case v == "of" || v == "off" || v == "0":
		return false, true
	case strings.HasPrefix("true", v) || strings.HasPrefix("yes", v):
		return true, true
	case strings.HasPrefix("false", v) || strings.HasPrefix("no", v):
		return false, true
	}
	return false, false
}
