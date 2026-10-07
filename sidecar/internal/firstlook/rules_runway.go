package firstlook

import (
	"fmt"
	"math"
	"time"
)

// xidWrapLimit is the distance to transaction ID wraparound (2^31 - 1).
const xidWrapLimit = 2_147_483_647

// TableAge is a table's age(relfrozenxid).
type TableAge struct {
	Schema, Table string
	Age           int64
}

// XIDState is the database's freeze horizon (pg_database).
type XIDState struct {
	Database        string
	DatFrozenXIDAge int64
	DatMinMXIDAge   int64
	FreezeMaxAge    int64
	OldestTables    []TableAge
}

// Sequence is one user sequence (pg_sequences) and the column it feeds.
type Sequence struct {
	Schema, Name string
	// LastValue is nil when the role may not read the sequence.
	LastValue   *int64
	Min, Max    int64
	Increment   int64
	Cycle       bool
	// OwnerColumn is the column the sequence feeds (owner, identity or
	// DEFAULT nextval; the narrowest when several), "" when none.
	OwnerColumn string
	OwnerType   string // format_type of OwnerColumn, "" when none
}

// TableStat is one table's size and tuple counters.
type TableStat struct {
	Schema, Table  string
	SizeBytes      int64
	Live, Dead     int64
	LastAutovacuum *time.Time
}

// Schema is one test-named schema and its traffic since the statistics
// window began (scans plus row writes on its tables).
type Schema struct {
	Name     string
	Tables   int
	Activity int64
}

// XIDRunway reports a database whose transaction ID or multixact horizon
// has used a large share of the distance to wraparound.
func XIDRunway(x XIDState, th Thresholds) []Item {
	var out []Item
	if sev := runwaySeverity(x.DatFrozenXIDAge, th); sev != "" {
		it := Item{Rule: RuleXIDRunway, Severity: sev, Object: x.Database,
			Title: fmt.Sprintf("Transaction ID runway: %s used",
				pct(float64(x.DatFrozenXIDAge)/xidWrapLimit)),
			Detail: fmt.Sprintf("age(datfrozenxid) = %d of %d before wraparound "+
				"(autovacuum_freeze_max_age = %d).%s", x.DatFrozenXIDAge, int64(xidWrapLimit),
				x.FreezeMaxAge, oldestText(x.OldestTables)),
			Recommendation: "Freeze the oldest tables and look for what holds the horizon " +
				"back: long transactions, prepared transactions, stale replication slots.",
			Evidence: []Evidence{{Source: "pg_database",
				Ref:    "age(pg_database.datfrozenxid) (datname " + x.Database + ")",
				Detail: fmt.Sprintf("%d", x.DatFrozenXIDAge)}}}
		if len(x.OldestTables) > 0 {
			o := x.OldestTables[0]
			it.SuggestedSQL = fmt.Sprintf("VACUUM (FREEZE, VERBOSE) %s.%s;", ident(o.Schema),
				ident(o.Table))
			it.Evidence = append(it.Evidence, Evidence{Source: "pg_class",
				Ref:    "age(pg_class.relfrozenxid) (relname " + qualified(o.Schema, o.Table) + ")",
				Detail: fmt.Sprintf("%d", o.Age)})
		}
		out = append(out, it)
	}
	if sev := runwaySeverity(x.DatMinMXIDAge, th); sev != "" {
		out = append(out, Item{Rule: RuleMultiXactRunway, Severity: sev, Object: x.Database,
			Title: fmt.Sprintf("Multixact runway: %s used",
				pct(float64(x.DatMinMXIDAge)/xidWrapLimit)),
			Detail: fmt.Sprintf("mxid_age(datminmxid) = %d of %d before multixact "+
				"wraparound.", x.DatMinMXIDAge, int64(xidWrapLimit)),
			Recommendation: "Freeze the tables with the oldest relminmxid; heavy row " +
				"locking (SELECT ... FOR SHARE, foreign keys) consumes multixacts.",
			Evidence: []Evidence{{Source: "pg_database",
				Ref:    "mxid_age(pg_database.datminmxid) (datname " + x.Database + ")",
				Detail: fmt.Sprintf("%d", x.DatMinMXIDAge)}}})
	}
	return out
}

func runwaySeverity(age int64, th Thresholds) string {
	switch {
	case age >= int64(th.XIDCriticalFraction*xidWrapLimit):
		return SeverityCritical
	case age >= int64(th.XIDWarnFraction*xidWrapLimit):
		return SeverityWarning
	}
	return ""
}

func oldestText(tables []TableAge) string {
	if len(tables) == 0 {
		return ""
	}
	o := tables[0]
	return fmt.Sprintf(" Oldest table: %s (age %d).", qualified(o.Schema, o.Table), o.Age)
}

// typeLimits are the value ranges of the integer column types.
var typeLimits = map[string][2]int64{
	"smallint": {math.MinInt16, math.MaxInt16},
	"integer":  {math.MinInt32, math.MaxInt32},
	"bigint":   {math.MinInt64, math.MaxInt64},
}

// SequenceRunway reports sequences that have used a large share of their
// range. The type of the column it feeds caps the range: a bigint sequence that
// feeds an integer column runs out at 2^31 - 1. unreadable counts the
// sequences the role may not read (pg_sequences.last_value is NULL).
func SequenceRunway(seqs []Sequence, th Thresholds) ([]Item, int) {
	var out []Item
	unreadable := 0
	for _, s := range seqs {
		if s.LastValue == nil {
			unreadable++
			continue
		}
		if s.Cycle || s.Increment == 0 {
			continue
		}
		used, limit, capped := sequenceUse(s)
		sev := ""
		switch {
		case used >= th.SequenceCriticalFraction:
			sev = SeverityCritical
		case used >= th.SequenceWarnFraction:
			sev = SeverityWarning
		}
		if sev != "" {
			out = append(out, sequenceItem(s, sev, used, limit, capped))
		}
	}
	return out, unreadable
}

// sequenceUse is the share of the range used, the effective limit and
// whether the fed column's type is what caps it.
func sequenceUse(s Sequence) (float64, int64, bool) {
	lim, hasType := typeLimits[s.OwnerType]
	last := float64(*s.LastValue)
	if s.Increment > 0 {
		limit, capped := s.Max, false
		if hasType && lim[1] < limit {
			limit, capped = lim[1], true
		}
		span := float64(limit) - float64(s.Min)
		if span <= 0 {
			return 1, limit, capped
		}
		return (last - float64(s.Min)) / span, limit, capped
	}
	limit, capped := s.Min, false
	if hasType && lim[0] > limit {
		limit, capped = lim[0], true
	}
	span := float64(s.Max) - float64(limit)
	if span <= 0 {
		return 1, limit, capped
	}
	return (float64(s.Max) - last) / span, limit, capped
}

func sequenceItem(s Sequence, sev string, used float64, limit int64, capped bool) Item {
	obj := qualified(s.Schema, s.Name)
	detail := fmt.Sprintf("last_value %d, limit %d: %s of the range used.", *s.LastValue,
		limit, pct(used))
	if capped {
		detail += fmt.Sprintf(" The column it feeds, %s, is %s, so the sequence runs out at "+
			"%d even though it allows more.", s.OwnerColumn, s.OwnerType, limit)
	} else if s.OwnerColumn != "" {
		detail += fmt.Sprintf(" It feeds %s (%s).", s.OwnerColumn, s.OwnerType)
	}
	rec := "Plan a wider key (or a higher MAXVALUE) before inserts start failing."
	if s.OwnerType == "integer" || s.OwnerType == "smallint" {
		rec = fmt.Sprintf("Migrate %s to bigint (a table rewrite: plan it) and widen the "+
			"sequence before inserts start failing.", s.OwnerColumn)
	}
	return Item{Rule: RuleSequenceRunway, Severity: sev, Object: obj,
		Title: fmt.Sprintf("Sequence %s is %s used", obj, pct(used)), Detail: detail,
		Recommendation: rec,
		Evidence: []Evidence{{Source: "pg_sequences",
			Ref: "pg_sequences.last_value, min_value, max_value (sequencename " + obj + ")",
			Detail: fmt.Sprintf("last_value=%d min=%d max=%d increment=%d", *s.LastValue,
				s.Min, s.Max, s.Increment)}}}
}

// TableBloat reports large tables with a high share of dead tuples. It is
// an estimate from the statistics counters, not a physical measurement.
func TableBloat(ts []TableStat, th Thresholds) []Item {
	var out []Item
	for _, t := range ts {
		total := t.Live + t.Dead
		if total <= 0 || t.SizeBytes < th.BloatMinBytes {
			continue
		}
		frac := float64(t.Dead) / float64(total)
		if frac < th.BloatDeadFraction {
			continue
		}
		sev := SeverityInfo
		if frac >= 0.5 {
			sev = SeverityWarning
		}
		obj := qualified(t.Schema, t.Table)
		out = append(out, Item{Rule: RuleTableBloat, Severity: sev, Object: obj,
			Title: fmt.Sprintf("Table %s: about %s dead tuples", obj, pct(frac)),
			Detail: fmt.Sprintf("%d dead of %d tuples; roughly %s of its %s is reclaimable "+
				"space.%s", t.Dead, total, sizeText(int64(float64(t.SizeBytes)*frac)),
				sizeText(t.SizeBytes), autovacuumText(t.LastAutovacuum)),
			Recommendation: "Let autovacuum catch up or run VACUUM; look for long " +
				"transactions that hold cleanup back.",
			SuggestedSQL: fmt.Sprintf("VACUUM (VERBOSE, ANALYZE) %s.%s;", ident(t.Schema),
				ident(t.Table)),
			Caveat: "An estimate from the dead-tuple counters, not a measurement; " +
				"pgstattuple measures it exactly.",
			Evidence: []Evidence{{Source: "pg_stat_user_tables",
				Ref:    "pg_stat_user_tables.n_dead_tup / n_live_tup (relname " + obj + ")",
				Detail: fmt.Sprintf("n_dead_tup=%d n_live_tup=%d", t.Dead, t.Live)}}})
	}
	return out
}

func autovacuumText(at *time.Time) string {
	if at == nil {
		return " Autovacuum has not processed it since the statistics window began."
	}
	return " Last autovacuum: " + at.UTC().Format(time.RFC3339) + "."
}

// TestSchemas lists test-named schemas. They become fact proposals (the
// operator confirms them as test fixtures); pg_sage never drops a schema.
func TestSchemas(ss []Schema, w StatsWindow, now time.Time) []Item {
	var out []Item
	window := "the statistics window began"
	if w.Known {
		window = fmt.Sprintf("%s (%s)", w.Since.UTC().Format("2006-01-02"),
			ageText(now.Sub(w.Since)))
	}
	for _, s := range ss {
		traffic := fmt.Sprintf("%d scans and row writes since %s", s.Activity, window)
		if s.Activity == 0 {
			traffic = "no scans or writes since " + window
		}
		out = append(out, Item{Rule: RuleTestSchema, Severity: SeverityInfo, Object: s.Name,
			Title:  "Test-named schema " + s.Name,
			Detail: fmt.Sprintf("Schema %s: %d tables, %s.", s.Name, s.Tables, traffic),
			Recommendation: "If it is a leftover of test runs, confirm the proposed fact " +
				"so pg_sage leaves it out of findings and budgets. pg_sage never drops it.",
			Evidence: []Evidence{{Source: "pg_namespace",
				Ref: "pg_namespace.nspname (test-name pattern); pg_stat_all_tables " +
					"(seq_scan, idx_scan, n_tup_ins, n_tup_upd, n_tup_del)",
				Detail: fmt.Sprintf("tables=%d activity=%d", s.Tables, s.Activity)}}})
	}
	return out
}
