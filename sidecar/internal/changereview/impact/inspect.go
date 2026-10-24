package impact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/changereview"
	"github.com/pg-sage/sidecar/internal/sqlast"
)

// InspectStatement inspects one statement (the reasoner's
// inspect_statement tool): it parses it, matches its fingerprint to the
// workload, plans it plan-only (a write as its read shape) and runs the
// rules on it. For DDL, which has no plan, Planned reports that the
// catalog analysis ran (every relation it targets was found or is created
// by it). An unparsable statement is an Inspection with ParseError, not
// an error; empty or multi-statement SQL, over sqlast.MaxInputBytes, or
// an unknown origin is ErrInvalid. Evidence ids are "inspect:<digest>"
// for the statement and shared with Analyze for the facts it cites.
func (e *Engine) InspectStatement(ctx context.Context, sql, origin string) (
	changereview.Inspection, error) {
	if err := validInspection(sql, origin); err != nil {
		return changereview.Inspection{}, err
	}
	r := e.newRun(ctx, e.d.Config, changereview.Input{
		SQLFiles: []changereview.SQLFile{{Content: sql}}})
	r.readVersion()
	ref := inspectRef(origin, sql)
	if !astAvailable() {
		return r.unparsed(sql, origin, ref, "the SQL parser (libpg_query) is not compiled "+
			"into this build"), nil
	}
	parts, err := inspectDDL(sql)
	if err != nil {
		return r.unparsed(sql, origin, ref, parseReason(err)), nil
	}
	if n := countStatements(parts); n != 1 {
		return changereview.Inspection{}, fmt.Errorf("%w: inspect one statement, got %d",
			changereview.ErrInvalid, n)
	}
	r.addParsed("", parts)
	s := r.stmts[0]
	s.evID = "inspect:" + ref
	r.analyzeStatements(nil)
	if r.err != nil {
		return changereview.Inspection{}, fmt.Errorf("inspect statement: %w", r.err)
	}
	ins := changereview.Inspection{Statement: r.statement(s), Parsed: true,
		Findings: r.finds, Evidence: r.ev, PlanError: s.planErr}
	ins.Statement.Origin = origin
	ins.Planned = r.planned(s)
	return ins, nil
}

func validInspection(sql, origin string) error {
	switch origin {
	case changereview.OriginSQLInput, changereview.OriginModelTranslated,
		changereview.OriginModelExtracted:
	default:
		return fmt.Errorf("%w: statement origin %q", changereview.ErrInvalid, origin)
	}
	if strings.TrimSpace(sql) == "" {
		return fmt.Errorf("%w: statement SQL is empty", changereview.ErrInvalid)
	}
	if len(sql) > sqlast.MaxInputBytes {
		return fmt.Errorf("%w: statement is over %d bytes", changereview.ErrInvalid,
			sqlast.MaxInputBytes)
	}
	return nil
}

func inspectRef(origin, sql string) string {
	sum := sha256.Sum256([]byte(origin + "\n" + sql))
	return hex.EncodeToString(sum[:6])
}

func countStatements(parts []sqlast.DDLStatement) int {
	n, last := 0, -1
	for _, p := range parts {
		if p.Statement != last {
			n, last = n+1, p.Statement
		}
	}
	return n
}

// planned reports whether s was planned: a read or a write's read shape
// has a plan; DDL counts when its relations are known.
func (r *run) planned(s *stmt) bool {
	switch s.kind {
	case "select", string(sqlast.DDLUpdate), string(sqlast.DDLDelete),
		string(sqlast.DDLInsert):
		return s.planSummary != ""
	}
	for i := range s.parts {
		if rel := r.target(s, i); rel != nil && !rel.found && !rel.created {
			return false
		}
	}
	return true
}

// unparsed is the inspection of a statement the parser rejected.
func (r *run) unparsed(sql, origin, ref, reason string) changereview.Inspection {
	s := &stmt{index: 1, sql: collapse(sql), kind: "unparsed", text: true,
		evID: "inspect:" + ref}
	r.stmts = append(r.stmts, s)
	r.statementEvidence(s)
	return changereview.Inspection{Statement: changereview.Statement{Index: 1,
		Origin: origin, SQL: s.sql, Kind: s.kind, EvidenceID: s.evID},
		ParseError: reason, Evidence: r.ev}
}
