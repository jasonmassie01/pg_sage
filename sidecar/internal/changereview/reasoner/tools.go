package reasoner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/pg-sage/sidecar/internal/agentloop"
	"github.com/pg-sage/sidecar/internal/changereview"
	"github.com/pg-sage/sidecar/internal/changereview/diffscan"
	"github.com/pg-sage/sidecar/internal/changereview/impact"
	"github.com/pg-sage/sidecar/internal/sqlast"
)

// The five closed, read-only tools. Arguments the reasoner refuses are
// agentloop.ErrInvalidArgs (nothing ran, nothing charged); engine failures
// are shown to the model as tool errors. Refusal texts never echo the
// model's arguments: they are shown outside the untrusted-data fence.

var columnArg = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_$]{0,62}$`)

func (s *session) tools() []agentloop.Tool {
	return []agentloop.Tool{
		{Name: ToolInspect, Cost: 1, Run: s.inspectTool, Parameters: json.RawMessage(
			inspectSchema), Description: "Parse one SQL statement, plan it plan-only (a " +
			"write as its read shape), match its fingerprint to the workload in " +
			"pg_stat_statements and run pg_sage's rules on it. origin: translated (from an " +
			"ORM migration) or extracted (from application code)."},
		{Name: ToolQueriesTouching, Cost: 0, Run: s.queriesTool, Parameters: json.RawMessage(
			queriesSchema), Description: "The workload statements that use a relation, or " +
			"one of its columns, busiest first, with calls and sqlcommenter tags."},
		{Name: ToolTableContext, Cost: 0, Run: s.tableTool, Parameters: json.RawMessage(
			tableSchema), Description: "A relation's live context: size, rows, write rate, " +
			"indexes with scans and plan users, dependent views, publications. Free for " +
			"relations the change touches; at most 3 others."},
		{Name: ToolWhatIf, Cost: 1, Run: s.whatIfTool, Parameters: json.RawMessage(
			whatIfSchema), Description: "Price one CREATE INDEX with session-local HypoPG " +
			"over statements you inspected (their inspect_statement evidence ids, at most " +
			"10), or over the table's workload reads when none are given."},
		{Name: ToolDiffExcerpt, Cost: 0, Run: s.excerptTool, Parameters: json.RawMessage(
			excerptSchema), Description: "Lines of a data-access file of the diff, new-side " +
			"numbers, redacted, at most 4 KB."},
	}
}

const (
	inspectSchema = `{"type":"object","properties":{"sql":{"type":"string",` +
		`"maxLength":100000},"origin":{"type":"string","enum":["translated","extracted"]}},` +
		`"required":["sql","origin"]}`
	queriesSchema = `{"type":"object","properties":{"relation":{"type":"string"},` +
		`"column":{"type":"string"}},"required":["relation"]}`
	tableSchema = `{"type":"object","properties":{"relation":{"type":"string"}},` +
		`"required":["relation"]}`
	whatIfSchema = `{"type":"object","properties":{"ddl":{"type":"string"},` +
		`"statement_evidence_ids":{"type":"array","maxItems":10,"items":{"type":"string"}}},` +
		`"required":["ddl"]}`
	excerptSchema = `{"type":"object","properties":{"path":{"type":"string"},` +
		`"from_line":{"type":"integer","minimum":1},"to_line":{"type":"integer",` +
		`"minimum":1}},"required":["path","from_line","to_line"]}`
)

func invalidArgs(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{agentloop.ErrInvalidArgs}, args...)...)
}

// decodeArgs reads a tool's arguments; a field of the wrong type refuses
// the call.
func decodeArgs(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(v); err != nil {
		return invalidArgs("arguments do not match the tool's schema")
	}
	return nil
}

// engineErr refuses arguments the engine rejected (without its message,
// which may quote them) and passes other failures on as tool errors.
func engineErr(err error) error {
	if errors.Is(err, changereview.ErrInvalid) {
		return invalidArgs("the impact engine refused the arguments")
	}
	return err
}

var origins = map[string]string{"translated": changereview.OriginModelTranslated,
	"extracted": changereview.OriginModelExtracted}

func (s *session) inspectTool(ctx context.Context, raw json.RawMessage) (agentloop.Output,
	error) {
	var a struct {
		SQL    string `json:"sql"`
		Origin string `json:"origin"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return agentloop.Output{}, err
	}
	origin, ok := origins[a.Origin]
	switch {
	case !ok:
		return agentloop.Output{}, invalidArgs("origin must be translated or extracted")
	case strings.TrimSpace(a.SQL) == "" || len(a.SQL) > sqlast.MaxInputBytes:
		return agentloop.Output{}, invalidArgs("sql must be one statement of at most %d "+
			"bytes", sqlast.MaxInputBytes)
	}
	ins, err := s.r.d.Tools.InspectStatement(ctx, a.SQL, origin)
	if err != nil {
		return agentloop.Output{}, engineErr(err)
	}
	// The model's label is not trusted: DDL it calls extracted is still
	// DDL it derived, never safe (CR-19).
	if ins.Parsed && changereview.IsModelDDL(changereview.Statement{Origin: origin,
		Kind: ins.Statement.Kind}) {
		origin = changereview.OriginModelTranslated
		ins.Statement.Origin = origin
	}
	rec := &inspection{sql: a.SQL, origin: origin, ins: ins,
		byID: map[string]changereview.Evidence{}}
	for _, e := range ins.Evidence {
		rec.byID[e.ID] = e
	}
	ev, label, err := enrichInspection(rec)
	if err != nil {
		return agentloop.Output{}, err
	}
	s.inspections[ev.ID] = rec
	for _, rel := range ins.Statement.Relations {
		s.touch(rel)
	}
	return s.cite(ev, label, label), nil
}

func (s *session) queriesTool(ctx context.Context, raw json.RawMessage) (agentloop.Output,
	error) {
	var a struct {
		Relation string `json:"relation"`
		Column   string `json:"column"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return agentloop.Output{}, err
	}
	if strings.TrimSpace(a.Relation) == "" || (a.Column != "" && !columnArg.MatchString(
		a.Column)) {
		return agentloop.Output{}, invalidArgs("relation must be [schema.]name and column " +
			"an identifier")
	}
	t, err := s.r.d.Tools.QueriesTouching(ctx, a.Relation, a.Column)
	if err != nil {
		return agentloop.Output{}, engineErr(err)
	}
	label := fmt.Sprintf("queries_touching: %d workload statements", len(t.Evidence))
	if !t.Known {
		label = "queries_touching: workload unknown (" + t.Reason + ")"
	}
	data := map[string]any{"tool": ToolQueriesTouching, "relation": t.Relation,
		"known": t.Known, "reason": t.Reason, "evidence": nested(t.Evidence),
		"args": map[string]string{"relation": a.Relation, "column": a.Column}}
	return s.wrap(ToolQueriesTouching, raw, label, data)
}

func (s *session) tableTool(ctx context.Context, raw json.RawMessage) (agentloop.Output,
	error) {
	var a struct {
		Relation string `json:"relation"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return agentloop.Output{}, err
	}
	if strings.TrimSpace(a.Relation) == "" {
		return agentloop.Output{}, invalidArgs("relation must be [schema.]name")
	}
	if !s.isTouched(a.Relation) {
		if s.untouched >= MaxUntouchedTables {
			return agentloop.Output{}, invalidArgs("at most %d relations the change does "+
				"not touch", MaxUntouchedTables)
		}
		s.untouched++
	}
	ev, err := s.r.d.Tools.TableContext(ctx, a.Relation)
	if err != nil {
		return agentloop.Output{}, engineErr(err)
	}
	data := map[string]any{"tool": ToolTableContext, "evidence": nested(ev),
		"args": map[string]string{"relation": a.Relation}}
	return s.wrap(ToolTableContext, raw, fmt.Sprintf("table_context: %d facts", len(ev)),
		data)
}

func (s *session) whatIfTool(ctx context.Context, raw json.RawMessage) (agentloop.Output,
	error) {
	var a struct {
		DDL string   `json:"ddl"`
		IDs []string `json:"statement_evidence_ids"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return agentloop.Output{}, err
	}
	if strings.TrimSpace(a.DDL) == "" || len(a.IDs) > impact.MaxWhatIfStatements {
		return agentloop.Output{}, invalidArgs("ddl must be one CREATE INDEX with at most "+
			"%d statement evidence ids", impact.MaxWhatIfStatements)
	}
	sqls := make([]string, 0, len(a.IDs))
	for _, ref := range a.IDs {
		rec := s.inspections[s.resolve(ref)]
		if rec == nil || !rec.ins.Parsed {
			return agentloop.Output{}, invalidArgs("every statement evidence id must be " +
				"a parsed inspect_statement result")
		}
		sqls = append(sqls, rec.sql)
	}
	ev, err := s.r.d.Tools.WhatIfIndex(ctx, a.DDL, sqls)
	if err != nil {
		return agentloop.Output{}, engineErr(err)
	}
	return s.cite(ev, ev.Label, ev.Label), nil
}

func (s *session) excerptTool(_ context.Context, raw json.RawMessage) (agentloop.Output,
	error) {
	var a struct {
		Path string `json:"path"`
		From int    `json:"from_line"`
		To   int    `json:"to_line"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return agentloop.Output{}, err
	}
	f, ok := diffscan.Find(s.files, a.Path)
	if !ok || !diffscan.Classify(f).DataAccess {
		return agentloop.Output{}, invalidArgs("path must be a data-access file of the diff")
	}
	text, err := diffscan.Excerpt(f, a.From, a.To, MaxExcerptBytes)
	if err != nil {
		return agentloop.Output{}, invalidArgs("no diff lines in that range (at most %d "+
			"lines)", diffscan.MaxExcerptLines)
	}
	label := fmt.Sprintf("diff excerpt, lines %d-%d", a.From, a.To)
	ev, err := changereview.NewEvidence("diff", shortHash(f.Path, fmt.Sprint(a.From),
		fmt.Sprint(a.To)), label, map[string]any{"path": f.Path, "from_line": a.From,
		"to_line": a.To, "text": text})
	if err != nil {
		return agentloop.Output{}, err
	}
	return s.cite(ev, label, label), nil
}

// wrap makes one tool result citable: "tool:<name>.<hash of its args>".
func (s *session) wrap(tool string, raw json.RawMessage, label string,
	data map[string]any) (agentloop.Output, error) {
	canon, err := changereview.CanonicalJSON(raw)
	if err != nil {
		return agentloop.Output{}, invalidArgs("arguments are not JSON")
	}
	ev, err := changereview.NewEvidence("tool", tool+"."+shortHash(string(canon)), label,
		data)
	if err != nil {
		return agentloop.Output{}, err
	}
	return s.cite(ev, label, label), nil
}

// nested is evidence as data inside a tool result (no digests: their hex
// would ground any number).
func nested(ev []changereview.Evidence) []map[string]any {
	out := make([]map[string]any, 0, len(ev))
	for _, e := range ev {
		out = append(out, map[string]any{"id": e.ID, "label": e.Label, "data": e.Data})
	}
	return out
}
