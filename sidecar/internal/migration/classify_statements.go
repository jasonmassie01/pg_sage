package migration

import (
	"context"
	"errors"
	"log"
	"regexp"
	"sync"

	"github.com/pg-sage/sidecar/internal/catalogread"
)

// StatementStatus says whether the classifier knows a statement.
type StatementStatus string

const (
	// StatementKnown: a DDL safety rule matched the statement, or the
	// classifier proved it a safe form.
	StatementKnown StatementStatus = "known"
	// StatementUnrecognized: no rule matched and the statement is not a
	// proven-safe form (or it did not parse). It is never proven safe.
	StatementUnrecognized StatementStatus = "unrecognized"
)

// ClassifiedStatement is one sanitized, whitespace-collapsed statement and
// what the classifier found in it. Classifications include the batch's
// ddl_missing_lock_timeout annotation, which is advice, not a hazard.
type ClassifiedStatement struct {
	Statement       string
	Status          StatementStatus
	Classifications []DDLClassification
}

// Classification is the result of ClassifyStatements for a SQL text.
type Classification struct {
	Statements []ClassifiedStatement
	// Untokenizable: the text ends inside a literal, quoted identifier,
	// dollar-quoted body or block comment, which can hide statements.
	Untokenizable bool
	// Degraded: the build has no SQL parser (no cgo), so the regex rules
	// and the additive allowlist classified the text.
	Degraded bool
}

// All returns every classification in statement order.
func (c Classification) All() []DDLClassification {
	var out []DDLClassification
	for _, s := range c.Statements {
		out = append(out, s.Classifications...)
	}
	return out
}

// Unrecognized counts the statements no rule or safe form recognized.
func (c Classification) Unrecognized() int {
	n := 0
	for _, s := range c.Statements {
		if s.Status == StatementUnrecognized {
			n++
		}
	}
	return n
}

// ClassifyStatements is the one entry point for DDL classification: the
// advisor, lint_migration and the agent gate all use it. It sanitizes and
// splits sql, classifies each statement with the parse-tree classifier
// (the regex rules on a build without cgo), and, when q is not nil, reads
// the column's current type to mark a binary-coercible ALTER COLUMN TYPE
// as no rewrite. version is server_version_num or a major (0 = unknown).
//
// The classification is complete even when err is not nil: err joins the
// column type reads that failed, and each such change is kept as a
// rewrite (the conservative answer).
func ClassifyStatements(
	ctx context.Context, q catalogread.Querier, sql string, version int,
) (Classification, error) {
	res := classifyText(sql, version, defaultStatementClassifier())
	if q == nil {
		return res, nil
	}
	var errs []error
	for i := range res.Statements {
		cs := res.Statements[i].Classifications
		for j := range cs {
			if err := refineAlterType(ctx, q, &cs[j]); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return res, errors.Join(errs...)
}

// NewClassifier returns the SQLParser the advisor uses: ClassifyStatements'
// statement classification (parse tree, or regex when degraded) without
// catalog reads; the advisor refines ALTER COLUMN TYPE with its own
// bounded reads.
func NewClassifier() SQLParser { return statementParser{} }

type statementParser struct{}

func (statementParser) Classify(sql string, version int) []DDLClassification {
	return classifyText(sql, version, defaultStatementClassifier()).All()
}

// statementClassifier classifies one sanitized, collapsed statement: the
// rules it matched, and whether the statement is known.
type statementClassifier func(stmt string, version int) ([]DDLClassification, bool)

// reBatchLockTimeout is a statement that sets this session's lock_timeout,
// which covers every later statement of the batch.
var reBatchLockTimeout = regexp.MustCompile(
	`(?i)^SET\s+(?:LOCAL\s+|SESSION\s+)?lock_timeout\b`)

// classifyText applies one statement classifier to every statement of
// sql and adds the batch's lock_timeout annotation.
func classifyText(sql string, version int, one statementClassifier) Classification {
	clean, closed := scanDDL(sql)
	res := Classification{Untokenizable: !closed, Degraded: !parserAvailable}
	lockTimeoutSet := false
	for _, stmt := range splitStatements(clean) {
		stmt = collapseWhitespace(stmt)
		cs := ClassifiedStatement{Statement: stmt, Status: StatementKnown}
		if reBatchLockTimeout.MatchString(stmt) {
			lockTimeoutSet = true
			res.Statements = append(res.Statements, cs)
			continue
		}
		found, known := one(stmt, version)
		if !known {
			cs.Status = StatementUnrecognized
		}
		if !lockTimeoutSet {
			found = append(found, lockTimeoutAnnotation(stmt, found)...)
		}
		cs.Classifications = found
		res.Statements = append(res.Statements, cs)
	}
	return res
}

// lockTimeoutAnnotation fires when any classification of the statement
// takes ACCESS EXCLUSIVE; the caller skips it when an earlier statement
// of the batch set lock_timeout.
func lockTimeoutAnnotation(stmt string, found []DDLClassification) []DDLClassification {
	for _, c := range found {
		if c.LockLevel == "ACCESS EXCLUSIVE" {
			lt := newClassification(catalogRule("ddl_missing_lock_timeout"), stmt)
			lt.TableName, lt.SchemaName = c.TableName, c.SchemaName
			return []DDLClassification{lt}
		}
	}
	return nil
}

// regexRules is the regex rule set, shared: it holds no per-call state.
var regexRules = NewRegexClassifier()

// regexStatement is the degraded classifier: the regex rules, with the
// additive allowlist as the only proof of a safe form (migration-04a).
func regexStatement(stmt string, _ int) ([]DDLClassification, bool) {
	found := regexRules.classifyStatement(stmt)
	return found, len(found) > 0 || provenAdditive(pgWhitespace.ReplaceAllString(stmt, " "))
}

// defaultStatementClassifier is the parse-tree classifier when the build
// has the SQL parser, else the regex rules (logged once as degraded).
func defaultStatementClassifier() statementClassifier {
	if parserAvailable {
		return astStatement
	}
	noteDegraded()
	return regexStatement
}

var (
	degradedOnce sync.Once
	// logDegraded reports the degraded build; tests replace it.
	logDegraded = func(msg string) { log.Print(msg) }
)

func noteDegraded() {
	degradedOnce.Do(func() {
		logDegraded("migration: DDL classification is degraded: this build has no SQL " +
			"parser (built without cgo), so only the regex rules and the additive " +
			"allowlist apply and other DDL is reported as unrecognized. Release builds " +
			"use CGO_ENABLED=1.")
	})
}

// catalogRule returns the catalog rule id; it panics on an id the
// catalog does not define (a programming error).
func catalogRule(id string) ruleDefinition {
	return regexRules.ruleByID(id)
}
