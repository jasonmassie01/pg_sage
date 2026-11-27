package provideradvice

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/optimizer"
)

// Finding is one of pg_sage's open index findings.
type Finding struct {
	ID       int64
	Category string
	// Object is the finding's object_identifier (schema.index for drops).
	Object string
	SQL    string
	// Constraint is the foreign key of a missing_fk_index finding.
	Constraint string
}

// IndexFact is what the catalog says about an index the provider wants
// dropped: whether it exists, the constraint it backs and its scans.
type IndexFact struct {
	Exists     bool
	Constraint string
	Scans      int64
}

// Evaluator is pg_sage's own judgement of provider-only advice: its
// HypoPG what-if for an index to create (would it act, and why) and the
// catalog facts of an index to drop.
type Evaluator interface {
	WhatIf(ctx context.Context, ddl string) (bool, string)
	IndexFact(ctx context.Context, index string) (IndexFact, error)
}

// Payload is a row's evidence. DDL is set only for a provider statement
// that parsed as exactly one CREATE INDEX; anything else stays
// ProviderText and is never offered as SQL.
type Payload struct {
	Object         string    `json:"object,omitempty"`
	DDL            string    `json:"ddl,omitempty"`
	ProviderText   string    `json:"provider_text,omitempty"`
	Constraint     string    `json:"constraint,omitempty"`
	Indexes        []string  `json:"indexes,omitempty"`
	ProviderReason string    `json:"provider_reason,omitempty"`
	PGSageSQL      string    `json:"pgsage_sql,omitempty"`
	WouldAct       bool      `json:"would_act"`
	ObservedAt     time.Time `json:"observed_at"`
}

// Row is one reconciled recommendation, keyed by ProviderID.
type Row struct {
	ProviderID string
	Kind       Kind
	Verdict    string
	Reason     string
	// FindingID is pg_sage's finding behind an agree or pgsage_only row.
	FindingID int64
	Payload   Payload
}

var (
	createCategories = map[string]bool{"missing_index": true, "missing_fk_index": true}
	dropCategories   = map[string]bool{"unused_index": true, "duplicate_index": true}
)

// Reconcile compares one provider's advice with pg_sage's open index
// findings. Every recommendation gives one row (repeated provider IDs
// count once). A finding no recommendation matched gives a pgsage_only
// row only when the provider gave index advice of a known kind: an empty
// list, an absent advisor or advice pg_sage cannot read says nothing about
// what the provider's advisor thinks of pg_sage's findings.
func Reconcile(ctx context.Context, recs []Recommendation, findings []Finding,
	ev Evaluator) []Row {
	var rows []Row
	matched, seenIDs := map[int64]bool{}, map[string]bool{}
	advised := false
	for _, rec := range recs {
		if rec.ProviderID == "" || seenIDs[rec.ProviderID] {
			continue
		}
		seenIDs[rec.ProviderID] = true
		row := reconcileOne(ctx, rec, findings, ev)
		if row.FindingID != 0 {
			matched[row.FindingID] = true
		}
		advised = advised || row.Kind != KindUnknown
		rows = append(rows, row)
	}
	if !advised {
		return rows
	}
	for _, f := range findings {
		if !matched[f.ID] && (createCategories[f.Category] || dropCategories[f.Category]) {
			rows = append(rows, pgsageOnly(f))
		}
	}
	return rows
}

func reconcileOne(ctx context.Context, rec Recommendation, findings []Finding,
	ev Evaluator) Row {
	row := Row{ProviderID: rec.ProviderID, Kind: rec.Kind, Payload: Payload{
		Object: rec.Object, Constraint: rec.Constraint, Indexes: rec.Indexes,
		ProviderReason: rec.Reason, ObservedAt: rec.ObservedAt}}
	switch rec.Kind {
	case KindCreateIndex:
		return reconcileCreate(ctx, row, rec, findings, ev)
	case KindDropIndex:
		row.Payload.ProviderText = rec.DDL
		return reconcileDrop(ctx, row, rec, findings, ev)
	}
	row.Kind, row.Verdict = KindUnknown, VerdictUnknown
	row.Reason = "advice of an unrecognized shape is shown, not parsed: " + rec.Reason
	return row
}

func reconcileCreate(ctx context.Context, row Row, rec Recommendation,
	findings []Finding, ev Evaluator) Row {
	if strings.TrimSpace(rec.DDL) == "" && rec.Constraint != "" {
		return reconcileForeignKey(row, rec, findings)
	}
	ddl, table, ok := parseCreate(rec.DDL)
	if !ok {
		row.Verdict, row.Payload.ProviderText = VerdictUnparsed, rec.DDL
		row.Reason = "the provider's statement is not exactly one CREATE INDEX pg_sage " +
			"can parse; it is kept as text and not offered as SQL"
		return row
	}
	row.Payload.DDL, row.Payload.Object = ddl, table
	for _, f := range findings {
		if createCategories[f.Category] && servesCreate(f.SQL, ddl, table) {
			return agree(row, f)
		}
	}
	row.Verdict = VerdictProviderOnly
	row.Payload.WouldAct, row.Reason = ev.WhatIf(ctx, ddl)
	return row
}

// servesCreate reports whether pg_sage's proposed index (findingSQL) on
// table serves every lookup the provider's index would: optimizer.CoveredBy,
// or Subsumes for a weaker partial-index predicate.
func servesCreate(findingSQL, ddl, table string) bool {
	own, ownTable, ok := parseCreate(findingSQL)
	if !ok || ownTable != table {
		return false
	}
	return optimizer.CoveredBy(ddl, own) || optimizer.Subsumes(own, ddl)
}

func reconcileForeignKey(row Row, rec Recommendation, findings []Finding) Row {
	for _, f := range findings {
		if f.Category != "missing_fk_index" || f.Constraint != rec.Constraint {
			continue
		}
		if _, table, ok := parseCreate(f.SQL); ok && table == rec.Object {
			return agree(row, f)
		}
	}
	row.Verdict = VerdictProviderOnly
	row.Reason = fmt.Sprintf("would not act: the provider names no index definition, "+
		"and pg_sage has no open missing_fk_index finding for constraint %s", rec.Constraint)
	return row
}

func reconcileDrop(ctx context.Context, row Row, rec Recommendation, findings []Finding,
	ev Evaluator) Row {
	if len(rec.Indexes) > 0 {
		return reconcileDuplicateSet(row, rec, findings)
	}
	if !strings.Contains(rec.Object, ".") {
		row.Verdict = VerdictUnparsed
		row.Reason = "the provider names no schema-qualified index to drop"
		return row
	}
	for _, f := range findings {
		if dropCategories[f.Category] && strings.EqualFold(f.Object, rec.Object) {
			return agree(row, f)
		}
	}
	fact, err := ev.IndexFact(ctx, rec.Object)
	row.Verdict = VerdictProviderOnly
	switch {
	case err != nil:
		row.Reason = "would not act: pg_sage could not read the index's catalog facts: " +
			err.Error()
	case !fact.Exists:
		row.Reason = "would not act: the index no longer exists"
	case fact.Constraint != "":
		row.Verdict = VerdictConflict
		row.Reason = fmt.Sprintf("the index backs constraint %s; dropping it drops the "+
			"constraint", fact.Constraint)
	case fact.Scans > 0:
		row.Verdict = VerdictConflict
		row.Reason = fmt.Sprintf("pg_sage sees %d scans of this index on this server",
			fact.Scans)
	default:
		row.Reason = "would not act: pg_sage has no open unused or duplicate finding for " +
			"this index; its own unused-index window and standby checks decide drops"
	}
	return row
}

func reconcileDuplicateSet(row Row, rec Recommendation, findings []Finding) Row {
	for _, f := range findings {
		if f.Category != "duplicate_index" {
			continue
		}
		for _, ix := range rec.Indexes {
			if strings.EqualFold(f.Object, ix) {
				return agree(row, f)
			}
		}
	}
	row.Verdict = VerdictProviderOnly
	row.Reason = "would not act: pg_sage has no open duplicate_index finding for " +
		strings.Join(rec.Indexes, ", ")
	return row
}

func agree(row Row, f Finding) Row {
	row.Verdict, row.FindingID, row.Payload.PGSageSQL = VerdictAgree, f.ID, f.SQL
	row.Reason = fmt.Sprintf("pg_sage's open %s finding %d proposes a change that "+
		"serves this", f.Category, f.ID)
	return row
}

func pgsageOnly(f Finding) Row {
	kind := KindCreateIndex
	if dropCategories[f.Category] {
		kind = KindDropIndex
	}
	return Row{ProviderID: "pgsage:" + strconv.FormatInt(f.ID, 10), Kind: kind,
		Verdict: VerdictPGSageOnly, FindingID: f.ID,
		Reason:  "pg_sage proposes this (" + f.Category + "); the provider's advisor does not",
		Payload: Payload{Object: f.Object, PGSageSQL: f.SQL}}
}

// parseCreate accepts exactly one CREATE INDEX statement and returns it
// without its terminator, with its table as schema.table (public when
// unqualified).
func parseCreate(raw string) (string, string, bool) {
	ddl := strings.TrimRight(strings.TrimSpace(raw), "; \t\r\n")
	if ddl == "" || strings.Contains(ddl, ";") {
		return "", "", false
	}
	spec, err := optimizer.ParseIndexDDL(ddl)
	if err != nil || spec.TableName == "" {
		return "", "", false
	}
	schema := spec.TableSchema
	if schema == "" {
		schema = "public"
	}
	return ddl, schema + "." + spec.TableName, true
}
