package impact

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"regexp"
	"strings"

	"github.com/pg-sage/sidecar/internal/changereview"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/sqlast"
)

// Rule ids of the deterministic catalog (CR-18 is changereview.RuleTextMode;
// CR-19 and CR-20 belong to the merge and the reasoner).
const (
	RuleLockAndRewrite     = "CR-01"
	RuleLockQueue          = "CR-02"
	RuleColumnReferenced   = "CR-03"
	RuleRelationReferenced = "CR-04"
	RuleWillFail           = "CR-05"
	RuleTypeChange         = "CR-06"
	RuleIndexUsed          = "CR-07"
	RuleIndexRedundant     = "CR-08"
	RuleIndexBenefit       = "CR-09"
	RuleFKUnindexed        = "CR-10"
	RuleFKTypeMismatch     = "CR-11"
	RuleBackfill           = "CR-12"
	RulePublished          = "CR-13"
	RuleStatementPlan      = "CR-14"
	RuleNewObject          = "CR-15"
	RulePartitioned        = "CR-16"
	RuleImpactUnknown      = "CR-17"
)

// ruleNames is each rule's kind, the Finding.Kind of its findings.
var ruleNames = map[string]string{
	RuleLockAndRewrite:        "lock_and_rewrite",
	RuleLockQueue:             "lock_queue_exposure",
	RuleColumnReferenced:      "column_still_referenced",
	RuleRelationReferenced:    "relation_still_referenced",
	RuleWillFail:              "change_will_fail",
	RuleTypeChange:            "type_change_impact",
	RuleIndexUsed:             "index_still_used",
	RuleIndexRedundant:        "index_redundant",
	RuleIndexBenefit:          "index_benefit",
	RuleFKUnindexed:           "fk_unindexed",
	RuleFKTypeMismatch:        "fk_type_mismatch",
	RuleBackfill:              "backfill_size",
	RulePublished:             "published_table",
	RuleStatementPlan:         "statement_plan",
	RuleNewObject:             "new_object_antipattern",
	RulePartitioned:           "partitioned_or_inherited",
	RuleImpactUnknown:         "impact_unknown",
	changereview.RuleTextMode: "unparsed_or_text_mode",
}

// newFinding is a rule finding citing evidence ids.
func newFinding(rule, severity, title, text string, ids ...string) changereview.Finding {
	return changereview.Finding{RuleID: rule, Kind: ruleNames[rule], Severity: severity,
		Origin: changereview.OriginRule, Title: title, Text: text,
		EvidenceIDs: append([]string{}, ids...)}
}

// Table lock modes a DDL statement takes.
const (
	lockAccessExclusive      = "ACCESS EXCLUSIVE"
	lockExclusive            = "EXCLUSIVE"
	lockShareRowExclusive    = "SHARE ROW EXCLUSIVE"
	lockShare                = "SHARE"
	lockShareUpdateExclusive = "SHARE UPDATE EXCLUSIVE"
)

// conflictsWith lists, per DDL lock mode, the workload statement modes it
// conflicts with (PostgreSQL's table-level lock conflict matrix, limited
// to the modes reads and writes take).
var conflictsWith = map[string][]sqlast.LockMode{
	lockAccessExclusive: {sqlast.LockAccessShare, sqlast.LockRowShare,
		sqlast.LockRowExclusive},
	lockExclusive:         {sqlast.LockRowShare, sqlast.LockRowExclusive},
	lockShareRowExclusive: {sqlast.LockRowExclusive},
	lockShare:             {sqlast.LockRowExclusive},
}

// conflicts reports whether a statement holding ddl blocks a workload
// statement that needs work.
func conflicts(ddl string, work sqlast.LockMode) bool {
	for _, m := range conflictsWith[ddl] {
		if m == work {
			return true
		}
	}
	return false
}

// statementLock is the table lock one statement part takes ("" for none
// that blocks the workload: CREATE TABLE, DML, settings).
func statementLock(p sqlast.DDLStatement) string {
	switch p.Kind {
	case sqlast.DDLCreateIndex:
		if p.Index != nil && p.Index.Concurrent {
			return lockShareUpdateExclusive
		}
		return lockShare
	case sqlast.DDLDropIndex:
		if p.Index != nil && p.Index.Concurrent {
			return lockShareUpdateExclusive
		}
		return lockAccessExclusive
	case sqlast.DDLAddConstraint:
		if p.Constraint != nil && p.Constraint.Kind == sqlast.ConstraintForeignKey {
			return lockShareRowExclusive
		}
		return lockAccessExclusive
	case sqlast.DDLValidateConstraint, sqlast.DDLCreateStatistics:
		return lockShareUpdateExclusive
	case sqlast.DDLAddColumn, sqlast.DDLDropColumn, sqlast.DDLRenameColumn,
		sqlast.DDLAlterType, sqlast.DDLSetNotNull, sqlast.DDLDropNotNull,
		sqlast.DDLSetDefault, sqlast.DDLDropConstraint, sqlast.DDLRenameTable,
		sqlast.DDLDropTable:
		return lockAccessExclusive
	case sqlast.DDLOther:
		return otherLock(p)
	}
	return ""
}

// otherLock is the table lock of a statement of kind other: ALTER TABLE
// and TRUNCATE take ACCESS EXCLUSIVE, LOCK TABLE the mode it names (""
// for one that blocks no read or write).
func otherLock(p sqlast.DDLStatement) string {
	switch p.Command {
	case "AlterTableStmt", "TruncateStmt":
		return lockAccessExclusive
	case "LockStmt":
		if _, blocks := conflictsWith[p.TableLock]; blocks {
			return p.TableLock
		}
	}
	return ""
}

// queueEstimate is how many statements queue behind a lock held holdMs
// at ratePerS conflicting calls per second.
func queueEstimate(ratePerS float64, holdMs int64) float64 {
	if ratePerS <= 0 || holdMs <= 0 {
		return 0
	}
	return ratePerS * float64(holdMs) / 1000
}

// waitingExposure bounds the statements that queue while the DDL itself
// waits for its lock: rate x lock_timeout; nil (unbounded) when the
// script leaves lock_timeout unset or 0 (disabled).
func waitingExposure(ratePerS float64, lockTimeoutMs *int64) *float64 {
	if lockTimeoutMs == nil || *lockTimeoutMs <= 0 {
		return nil
	}
	v := max(ratePerS, 0) * float64(*lockTimeoutMs) / 1000
	return &v
}

// indexBuildBytesPerSecond is the rule's index build speed: 50 MiB/s.
const indexBuildBytesPerSecond = 50 << 20

// indexBuildHoldMs is a plain CREATE INDEX's SHARE lock hold over a heap
// of heapBytes (0 when the size is unknown).
func indexBuildHoldMs(heapBytes int64) int64 {
	if heapBytes <= 0 {
		return 0
	}
	return int64(math.Round(float64(heapBytes) / indexBuildBytesPerSecond * 1000))
}

func queueSeverity(queued float64, cfg config.ChangeReviewConfig) string {
	switch {
	case queued >= float64(cfg.LockQueueCritical):
		return changereview.SeverityCritical
	case queued >= float64(cfg.LockQueueWarn):
		return changereview.SeverityWarning
	}
	return changereview.SeverityInfo
}

func backfillSeverity(rows float64, cfg config.ChangeReviewConfig) string {
	switch {
	case rows >= float64(cfg.BackfillRowsCritical):
		return changereview.SeverityCritical
	case rows >= float64(cfg.BackfillRowsWarn):
		return changereview.SeverityWarning
	}
	return changereview.SeverityInfo
}

// typeNames maps libpg_query's internal type names, and the serial
// pseudo-types (which it keeps as written), to format_type's.
var typeNames = map[string]string{
	"int2": "smallint", "int4": "integer", "int8": "bigint", "bool": "boolean",
	"float4": "real", "float8": "double precision", "varchar": "character varying",
	"bpchar": "character", "timestamptz": "timestamp with time zone",
	"timestamp": "timestamp without time zone", "timetz": "time with time zone",
	"time": "time without time zone", "varbit": "bit varying",
	"serial": "integer", "serial4": "integer", "bigserial": "bigint", "serial8": "bigint",
	"smallserial": "smallint", "serial2": "smallint",
}

var typeWithMod = regexp.MustCompile(`^([a-z0-9_ ]+?)(\([0-9, ]+\))?((?:\[\])*)$`)

// formatType renders an InspectDDL type name ("varchar(20)") as
// format_type does ("character varying(20)"), so post-conditions compare
// against the catalog.
func formatType(t string) string {
	m := typeWithMod.FindStringSubmatch(strings.TrimSpace(t))
	if m == nil {
		return t
	}
	base := m[1]
	if name, ok := typeNames[base]; ok {
		base = name
	}
	mod := strings.ReplaceAll(m[2], " ", "")
	if mod != "" && strings.HasPrefix(base, "time") {
		return strings.Replace(base, " ", mod+" ", 1) + m[3]
	}
	return base + mod + m[3]
}

var refUnsafe = regexp.MustCompile(`[^!-~]`)

// evidenceRef makes s an evidence reference: printable ASCII without
// spaces, at most 120 bytes. A name that needed changing gets a short
// digest of the original, so distinct names never collide.
func evidenceRef(s string) string {
	clean := refUnsafe.ReplaceAllString(s, "_")
	if clean == s && s != "" && len(s) <= 120 {
		return s
	}
	sum := sha256.Sum256([]byte(s))
	if len(clean) > 100 {
		clean = clean[:100]
	}
	return clean + "~" + hex.EncodeToString(sum[:4])
}
