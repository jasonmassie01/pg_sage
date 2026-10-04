package sre

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/agentloop"
	"github.com/pg-sage/sidecar/internal/sre/causal"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// The investigator's tools (roadmap 2.1, owner decision 1). All read:
// catalog probes and pg_stat views through the database's one probe
// runner (its per-database and sidecar limiters, its caps and read-only
// transactions), a plan-only EXPLAIN by queryid, the causal graph's
// current state and the operator-confirmed facts. Probe results are
// committed as investigation evidence before the model sees them, so
// every call is cited by its stored id and digest. The model names
// catalog ids and typed arguments; it never supplies SQL.

const (
	maxResultRows  = 20
	maxResultRunes = 4000
)

func (s *investigatorSession) tools() []agentloop.Tool {
	var out []agentloop.Tool
	for _, name := range s.plan.Tools {
		switch name {
		case ToolRunProbe:
			out = append(out, s.probeTool())
		case ToolStatView:
			out = append(out, s.statTool())
		case ToolExplain:
			if s.c.investigator.Explainer != nil {
				out = append(out, s.explainTool())
			}
		case ToolGraphState:
			out = append(out, s.graphTool())
		case ToolFacts:
			out = append(out, s.factsTool())
		}
	}
	return out
}

func toolNames(ts []agentloop.Tool) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Name)
	}
	return out
}

func (s *investigatorSession) probeTool() agentloop.Tool {
	ids := make([]string, 0, len(probes.Catalog().IDs()))
	for _, id := range probes.Catalog().IDs() {
		ids = append(ids, `"`+string(id)+`"`)
	}
	schema := `{"type":"object","properties":{"probe":{"type":"string","enum":[` +
		strings.Join(ids, ",") + `]},"args":{"type":"object","properties":{` +
		`"pid":{"type":"integer"},"backend_start":{"type":"string"},` +
		`"window_seconds":{"type":"integer"}}}},"required":["probe"]}`
	return agentloop.Tool{Name: ToolRunProbe, Cost: 1, Parameters: json.RawMessage(schema),
		Description: "Run one read-only catalog probe with its typed arguments; the " +
			"result is citable evidence.",
		Run: func(ctx context.Context, raw json.RawMessage) (agentloop.Output, error) {
			id, args, err := decodeProbeCall(raw)
			if err != nil {
				return agentloop.Output{}, fmt.Errorf("%w: %v", agentloop.ErrInvalidArgs, err)
			}
			return s.collect(ctx, func(ctx context.Context) probes.Result {
				return s.c.runner.Run(ctx, id, args)
			})
		}}
}

func decodeProbeCall(raw json.RawMessage) (probes.ID, probes.Args, error) {
	var call struct {
		Probe string          `json:"probe"`
		Args  json.RawMessage `json:"args"`
	}
	if err := json.Unmarshal(raw, &call); err != nil {
		return "", probes.Args{}, fmt.Errorf("arguments: %v", err)
	}
	id := probes.ID(call.Probe)
	if _, ok := probes.Catalog().Spec(id); !ok {
		return "", probes.Args{}, fmt.Errorf("%q is not a catalog probe", call.Probe)
	}
	args, err := decodeProbeArgs(call.Args)
	if err == nil {
		err = probes.Catalog().CheckArgs(id, args)
	}
	return id, args, err
}

func (s *investigatorSession) statTool() agentloop.Tool {
	views := make([]string, 0, 3)
	for _, v := range probes.StatViews() {
		views = append(views, `"`+v+`"`)
	}
	schema := `{"type":"object","properties":{"view":{"type":"string","enum":[` +
		strings.Join(views, ",") + `]}},"required":["view"]}`
	return agentloop.Tool{Name: ToolStatView, Cost: 1, Parameters: json.RawMessage(schema),
		Description: "Read one pg_stat view: database counters, the busiest tables or " +
			"the top statements by time; the result is citable evidence.",
		Run: func(ctx context.Context, raw json.RawMessage) (agentloop.Output, error) {
			var a struct {
				View string `json:"view"`
			}
			id := probes.ID("")
			if json.Unmarshal(raw, &a) == nil {
				id = probes.StatView(a.View)
			}
			if id == "" {
				return agentloop.Output{}, fmt.Errorf("%w: view must be one of %s",
					agentloop.ErrInvalidArgs, strings.Join(probes.StatViews(), ", "))
			}
			return s.collect(ctx, func(ctx context.Context) probes.Result {
				return s.c.runner.Run(ctx, id, probes.Args{})
			})
		}}
}

func (s *investigatorSession) explainTool() agentloop.Tool {
	return agentloop.Tool{Name: ToolExplain, Cost: 1,
		Parameters: json.RawMessage(`{"type":"object","properties":{"queryid":` +
			`{"type":"integer"}},"required":["queryid"]}`),
		Description: "Plan-only EXPLAIN of one statement named by its pg_stat_statements " +
			"queryid (never ANALYZE); the plan's node shapes are citable evidence.",
		Run: func(ctx context.Context, raw json.RawMessage) (agentloop.Output, error) {
			qid, err := decodeQueryID(raw)
			if err != nil {
				return agentloop.Output{}, fmt.Errorf("%w: %v", agentloop.ErrInvalidArgs, err)
			}
			return s.collect(ctx, func(ctx context.Context) probes.Result {
				return s.c.investigator.Explainer.ExplainStatement(ctx, qid)
			})
		}}
}

func decodeQueryID(raw json.RawMessage) (int64, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var a struct {
		QueryID json.Number `json:"queryid"`
	}
	if err := dec.Decode(&a); err != nil {
		return 0, fmt.Errorf("queryid must be an integer")
	}
	qid, err := a.QueryID.Int64()
	if err != nil || qid == 0 {
		return 0, fmt.Errorf("queryid must be a non-zero integer")
	}
	return qid, nil
}

func (s *investigatorSession) graphTool() agentloop.Tool {
	return agentloop.Tool{Name: ToolGraphState,
		Parameters: json.RawMessage(`{"type":"object","properties":{}}`),
		Description: "The causal graph's diagnosis on all evidence collected so far " +
			"(not citable: cite the evidence it rests on).",
		Run: func(ctx context.Context, _ json.RawMessage) (agentloop.Output, error) {
			d, _, err := s.diagnosis(ctx)
			if err != nil {
				return agentloop.Output{}, agentloop.Abort(err)
			}
			return agentloop.Output{Status: "ok", Text: graphText(d)}, nil
		}}
}

func graphText(d causal.Diagnosis) string {
	var b strings.Builder
	if root := graphRootNode(d); root != "" {
		fmt.Fprintf(&b, "root cause %s (conclusive)\n", root)
	} else {
		fmt.Fprintf(&b, "inconclusive: %s\n", RedactText(d.Reason))
	}
	writeHypothesisLines(&b, reviewScope{diagnosis: d}.openHypotheses())
	b.WriteString("ruled out:\n")
	writeHypothesisLines(&b, d.RuledOut)
	for _, m := range d.Missing {
		fmt.Fprintf(&b, "missing: %s %s %s\n", m.ProbeID, m.Status, RedactText(m.Reason))
	}
	return b.String()
}

func (s *investigatorSession) factsTool() agentloop.Tool {
	return agentloop.Tool{Name: ToolFacts,
		Parameters: json.RawMessage(`{"type":"object","properties":{}}`),
		Description: "The operator-confirmed facts about this database (context, " +
			"not citable).",
		Run: func(ctx context.Context, _ json.RawMessage) (agentloop.Output, error) {
			facts := s.c.promptFacts(ctx)
			if facts == "" {
				facts = "No operator-confirmed facts for this database."
			}
			return agentloop.Output{Status: "ok", Text: facts}, nil
		}}
}

// diagnosis re-runs the causal graph on everything stored so far.
func (s *investigatorSession) diagnosis(ctx context.Context) (causal.Diagnosis,
	[]Evidence, error) {
	stored, err := s.c.store.Evidence(ctx, s.lease.Scope, s.inv.ID)
	if err != nil {
		return causal.Diagnosis{}, nil, err
	}
	obs, err := observations(stored)
	if err != nil {
		return causal.Diagnosis{}, nil, err
	}
	return diagnose(s.inv, obs), stored, nil
}

// collect runs one read under the lease, commits its result as evidence
// and returns it as the model sees it. Running out of active time or of
// the investigation's probe ceiling is a tool failure the model is told
// about; a lost lease or an unavailable store aborts the run.
func (s *investigatorSession) collect(ctx context.Context,
	read func(context.Context) probes.Result) (agentloop.Output, error) {
	if time.Until(s.lease.SegmentDeadline) < 2*stepMargin {
		return agentloop.Output{}, errors.New("no active time is left for another read")
	}
	lease, err := s.c.store.Heartbeat(ctx, s.lease)
	if err != nil {
		return agentloop.Output{}, agentloop.Abort(err)
	}
	s.lease = lease
	res := read(ctx)
	ev, err := s.commit(ctx, res)
	if errors.Is(err, ErrBudgetExhausted) {
		return agentloop.Output{}, fmt.Errorf("the investigation's probe ceiling is "+
			"reached: %v", err)
	}
	if err != nil {
		return agentloop.Output{}, agentloop.Abort(err)
	}
	text := resultForModel(res)
	return agentloop.Output{Status: string(res.Status), Text: text,
		Evidence: &agentloop.Evidence{ID: string(ev.ID), Digest: hex.EncodeToString(ev.SHA256),
			Label: string(res.ProbeID) + " " + string(res.Status), Text: text}}, nil
}

// commit stores one result as a step (evaluating -> needs_evidence ->
// collecting -> evaluating) and returns the stored evidence.
func (s *investigatorSession) commit(ctx context.Context, res probes.Result) (Evidence,
	error) {
	s.commits++
	key := fmt.Sprintf("inv-read-f%d-%d", s.lease.Fence, s.commits)
	steps := []StepResult{{IdempotencyKey: key, Results: []probes.Result{res},
		NextState: StateNeedsEvidence},
		{IdempotencyKey: key + "-collect", NextState: StateCollecting},
		{IdempotencyKey: key + "-evaluate", NextState: StateEvaluating}}
	for _, st := range steps {
		if _, err := s.c.store.CommitStep(ctx, s.lease, st); err != nil {
			return Evidence{}, err
		}
	}
	stored, err := s.c.store.Evidence(ctx, s.lease.Scope, s.inv.ID)
	if err != nil {
		return Evidence{}, err
	}
	for _, e := range stored {
		if e.StepKey == key {
			return e, nil
		}
	}
	return Evidence{}, fmt.Errorf("%w: the committed read %s is not stored", ErrNotFound,
		key)
}

// resultForModel is a result as the model sees it: every text value
// scrubbed of secrets and PII-like literals, at most maxResultRows rows.
func resultForModel(res probes.Result) string {
	clean := res
	clean.Reason, clean.Error = scrub(res.Reason), scrub(res.Error)
	clean.Rows = make([]probes.Row, 0, len(res.Rows))
	for _, row := range res.Rows {
		r := make(probes.Row, len(row))
		for k, v := range row {
			if str, ok := v.(string); ok {
				v = scrub(str)
			}
			r[k] = v
		}
		clean.Rows = append(clean.Rows, r)
	}
	text := clean.Text(maxResultRows)
	if res.Error != "" {
		text += "\nerror: " + clean.Error
	}
	return truncateRunes(text, maxResultRunes)
}
