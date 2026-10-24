package receipt

import (
	"encoding/json"
	"fmt"

	"github.com/pg-sage/sidecar/internal/changereview"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/sqlast"
)

// Shadow auto-clear. Every completed review records whether it would have
// cleared without a DBA (would_auto_clear) by a deterministic rule; the
// DBA's verdict and the receipt score it later. Shadow only: nothing reads
// the flag to decide a verdict, a CI status, a receipt or a credit.

// AutoClear is changereview.Deps.AutoClear for cfg: Eligible with the
// configured auto_clear_max_table_bytes.
func AutoClear(cfg config.ChangeReviewConfig) func(changereview.Review) bool {
	limit := cfg.AutoClearMaxTableBytes
	return func(r changereview.Review) bool {
		ok, _ := Eligible(r, limit)
		return ok
	}
}

// Eligible reports whether r would auto-clear, and when not, why. It needs
// a safe AST verdict, no model finding, no model-translated DDL, no CR-17,
// CR-18, CR-19 or CR-20 finding, and only allowlisted statements, each
// parsed with libpg_query: CREATE INDEX CONCURRENTLY on a plain
// (non-partitioned, non-inherited) table of at most maxTableBytes;
// nullable ADD COLUMN without a volatile default or constraint; CREATE
// TABLE without a foreign key, partition or inheritance; CREATE
// STATISTICS; COMMENT; and the SET and transaction statements around them.
func Eligible(r changereview.Review, maxTableBytes int64) (bool, string) {
	if why := reviewBlocks(r); why != "" {
		return false, why
	}
	tables := tableEvidence(r.Evidence)
	for _, s := range r.Statements {
		if s.Origin != changereview.OriginSQLInput {
			continue
		}
		if why := statementBlocks(s, tables, maxTableBytes); why != "" {
			return false, fmt.Sprintf("statement %d: %s", s.Index, why)
		}
	}
	return true, ""
}

// blockingRules make a review ineligible: unknown impact, text mode,
// model-translated DDL, SQL the review could not analyze.
var blockingRules = map[string]bool{"CR-17": true, changereview.RuleTextMode: true,
	changereview.RuleModelTranslated: true, changereview.RuleNotAnalyzableSQL: true}

// reviewBlocks is why the review as a whole is ineligible ("" if not).
func reviewBlocks(r changereview.Review) string {
	switch {
	case r.Verdict != changereview.VerdictSafe:
		return "verdict " + r.Verdict
	case r.ParseMode != changereview.ParseAST:
		return "parse mode " + r.ParseMode
	}
	for _, f := range r.Findings {
		if f.Origin == changereview.OriginModel {
			return "model finding " + f.Title
		}
		if blockingRules[f.RuleID] {
			return "finding " + f.RuleID
		}
	}
	inputs := 0
	for _, s := range r.Statements {
		switch {
		case s.Origin == changereview.OriginModelTranslated:
			return fmt.Sprintf("statement %d is model-translated", s.Index)
		case changereview.IsModelDDL(s):
			return fmt.Sprintf("statement %d is DDL the model derived", s.Index)
		case s.Origin == changereview.OriginSQLInput:
			inputs++
		}
	}
	if inputs == 0 {
		return "no statements"
	}
	return ""
}

// tableFacts are the table evidence fields eligibility reads.
type tableFacts struct {
	Kind       string `json:"kind"`
	TotalBytes int64  `json:"total_bytes"`
	Partitions int    `json:"partitions"`
	Children   int    `json:"inheritance_children"`
}

// tableEvidence maps "schema.name" to the review's table: evidence.
func tableEvidence(evs []changereview.Evidence) map[string]tableFacts {
	out := map[string]tableFacts{}
	for _, e := range evs {
		if e.Kind != "table" || len(e.ID) <= len("table:") {
			continue
		}
		var t tableFacts
		if json.Unmarshal(e.Data, &t) == nil {
			out[e.ID[len("table:"):]] = t
		}
	}
	return out
}

// statementBlocks is why one input statement is not allowlisted.
func statementBlocks(s changereview.Statement, tables map[string]tableFacts,
	maxTableBytes int64) string {
	parts, err := sqlast.InspectDDL(s.SQL)
	switch {
	case err != nil && sqlast.Available():
		return "does not parse: " + err.Error()
	case err != nil:
		return "parser unavailable (a nocgo build)"
	}
	for _, p := range parts {
		if why := partBlocks(p, s, tables, maxTableBytes); why != "" {
			return why
		}
	}
	return ""
}

func partBlocks(p sqlast.DDLStatement, s changereview.Statement,
	tables map[string]tableFacts, maxTableBytes int64) string {
	switch p.Kind {
	case sqlast.DDLCreateIndex:
		return indexBlocks(p, s, tables, maxTableBytes)
	case sqlast.DDLAddColumn:
		return columnBlocks(p.Column)
	case sqlast.DDLCreateTable:
		return tableBlocks(p.Table)
	case sqlast.DDLCreateStatistics, sqlast.DDLSet, sqlast.DDLTransaction:
		return ""
	case sqlast.DDLOther:
		if p.Command == "CommentStmt" {
			return ""
		}
		return p.Command + " is not allowlisted"
	}
	return string(p.Kind) + " is not allowlisted"
}

func indexBlocks(p sqlast.DDLStatement, s changereview.Statement,
	tables map[string]tableFacts, maxTableBytes int64) string {
	if p.Index == nil || !p.Index.Concurrent {
		return "an index build without CONCURRENTLY"
	}
	if len(s.Relations) != 1 {
		return "an index build on an unresolved table"
	}
	rel := s.Relations[0]
	t, ok := tables[rel]
	switch {
	case !ok:
		return "no table evidence for " + rel
	case t.Kind != "table" || t.Partitions > 0 || t.Children > 0:
		return fmt.Sprintf("%s is %s (partitioned or inherited tables are excluded)", rel,
			t.Kind)
	case t.TotalBytes > maxTableBytes:
		return fmt.Sprintf("%s is %d bytes, over the %d-byte limit", rel, t.TotalBytes,
			maxTableBytes)
	}
	return ""
}

func columnBlocks(c *sqlast.ColumnDef) string {
	switch {
	case c == nil:
		return "an ADD COLUMN without a column"
	case c.NotNull:
		return "ADD COLUMN " + c.Name + " NOT NULL"
	case c.DefaultVolatile || c.Generated != "":
		return "ADD COLUMN " + c.Name + " with a volatile default"
	case len(c.Constraints) > 0:
		return "ADD COLUMN " + c.Name + " with a constraint"
	}
	return ""
}

func tableBlocks(t *sqlast.TableDef) string {
	switch {
	case t == nil:
		return ""
	case t.PartitionOf != nil || t.PartitionBy != "" || len(t.Inherits) > 0:
		return "CREATE TABLE with a partition or inheritance"
	}
	for _, c := range t.Constraints {
		if c.Kind == sqlast.ConstraintForeignKey {
			return "CREATE TABLE with a foreign key"
		}
	}
	return ""
}

// Score is the shadow score of a flagged review from its DBA verdict and
// receipt: correct for concur with a clean receipt; incorrect for an
// override, concur_with_edits or a regressed receipt; unscored otherwise
// (pending, or an unverifiable receipt).
func Score(dbaVerdict, receiptVerdict string) string {
	switch {
	case dbaVerdict == changereview.DBAOverride || dbaVerdict == changereview.DBAConcurWithEdits,
		receiptVerdict == changereview.ReceiptRegressed:
		return changereview.ShadowIncorrect
	case dbaVerdict == changereview.DBAConcur &&
		(receiptVerdict == changereview.ReceiptClean ||
			receiptVerdict == changereview.ReceiptCleanLowTraffic):
		return changereview.ShadowCorrect
	}
	return changereview.ShadowUnscored
}
