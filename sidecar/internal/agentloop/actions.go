package agentloop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/llm"
)

// Replies become actions: native tool calls, or one JSON action in the
// content (the JSON protocol, and native providers that answer in
// content). The final action ends the run; every other action is a tool
// call checked against the budgets before it runs.

type action struct {
	id   string // native tool call id; "" for a JSON action
	name string
	args json.RawMessage
}

type parsedReply struct {
	actions []action
	plan    string
	bad     string // rejection reason when the reply has no usable action
	detail  string
}

func parseReply(res llm.ToolResult) parsedReply {
	if len(res.ToolCalls) > 0 {
		out := parsedReply{plan: res.Content}
		for _, tc := range res.ToolCalls {
			out.actions = append(out.actions, action{id: tc.ID, name: tc.Name,
				args: objectOrEmpty(tc.Arguments)})
		}
		return out
	}
	return parseJSONAction(res.Content)
}

func parseJSONAction(content string) parsedReply {
	if strings.TrimSpace(content) == "" {
		return parsedReply{bad: RejectEmptyReply, detail: "the reply is empty"}
	}
	cleaned := strings.TrimSpace(llm.StripJSON(content, llm.JSONObject))
	var a struct {
		Tool string          `json:"tool"`
		Args json.RawMessage `json:"args"`
		Plan string          `json:"plan"`
		Note string          `json:"note"`
	}
	if err := json.Unmarshal([]byte(cleaned), &a); err != nil || a.Tool == "" {
		return parsedReply{bad: RejectMalformedReply, detail: "not a JSON action " +
			`{"tool": "<name>", "args": {...}}`}
	}
	args := objectOrEmpty(a.Args)
	if !schemaObject(args) {
		return parsedReply{bad: RejectMalformedReply, detail: "args of " + a.Tool +
			" is not a JSON object"}
	}
	plan := a.Plan
	if plan == "" {
		plan = a.Note
	}
	return parsedReply{actions: []action{{name: a.Tool, args: args}}, plan: plan}
}

func objectOrEmpty(raw json.RawMessage) json.RawMessage {
	if s := strings.TrimSpace(string(raw)); s == "" || s == "null" {
		return json.RawMessage(`{}`)
	}
	return raw
}

// handle runs a reply's actions; true ends the run.
func (r *run) handle(ctx context.Context, res llm.ToolResult, finalOnly bool) (bool, error) {
	rep := parseReply(res)
	if p := oneLine(rep.plan); p != "" && r.res.Transcript.Plan == "" {
		r.res.Transcript.Plan = clip(p, maxPlanRunes)
	}
	if rep.bad != "" {
		return r.badReply(rep.bad, rep.detail), nil
	}
	r.bad = 0
	r.msgs = append(r.msgs, llm.Message{Role: "assistant", Content: res.Content,
		ToolCalls: res.ToolCalls})
	for i, a := range rep.actions {
		if a.name == r.cfg.Final.Name {
			r.finish(rep.actions, i)
			return true, nil
		}
	}
	for _, a := range rep.actions {
		if err := r.call(ctx, a, finalOnly); err != nil {
			return true, err
		}
	}
	return false, nil
}

// finish takes the final answer: other actions of the same reply are
// refused, its claims go through the citation filter.
func (r *run) finish(actions []action, final int) {
	for i, a := range actions {
		if i != final {
			r.res.Transcript.Rejected[RejectBesideFinal]++
			r.record(Step{Tool: a.name, Args: argsOf(a.args), Status: StatusRejected,
				Note: RejectBesideFinal})
		}
	}
	args := actions[final].args
	claims, malformed := decodeClaims(args)
	kept, dropped := FilterClaims(claims, r.aliases, r.cfg.Ground, r.cfg.MaxClaims)
	r.res.Final, r.res.Claims = args, kept
	r.res.Dropped = append(malformed, dropped...)
	r.record(Step{Tool: r.cfg.Final.Name, Status: StatusFinal})
	r.stop(StopFinal, "")
}

// decodeClaims reads the final answer's claims one by one; a claim that
// does not decode is dropped as malformed.
func decodeClaims(args json.RawMessage) ([]Claim, []DroppedClaim) {
	var body struct {
		Claims []json.RawMessage `json:"claims"`
	}
	if json.Unmarshal(args, &body) != nil {
		return nil, nil
	}
	var out []Claim
	var bad []DroppedClaim
	for _, raw := range body.Claims {
		var c Claim
		if err := json.Unmarshal(raw, &c); err != nil {
			bad = append(bad, DroppedClaim{Text: clip(string(raw), 200), Reason: DropMalformed})
			continue
		}
		out = append(out, c)
	}
	return out, bad
}

// call checks one tool call against the budgets, runs it and answers.
func (r *run) call(ctx context.Context, a action, finalOnly bool) error {
	s := Step{Tool: a.name, Args: argsOf(a.args)}
	t, ok := r.tool(a.name)
	if !ok {
		r.refuse(a, s, RejectForbiddenTool, a.name+" is not a listed tool")
		return nil
	}
	r.res.Transcript.ToolCalls++
	key := a.name + canonical(a.args)
	prev, dup := r.done[key]
	switch {
	case finalOnly || r.res.Transcript.ToolCalls > r.cfg.Budget.MaxCalls:
		r.refuse(a, s, RejectCallBudget, "no tool calls are left")
	case dup:
		r.refuse(a, s, RejectDuplicate, "the same call already ran"+aliasNote(prev))
	case t.Cost > 0 && r.res.Transcript.Cost+t.Cost > r.cfg.Budget.MaxCost:
		r.refuse(a, s, RejectCostBudget, fmt.Sprintf("%d of %d probe cost units are used",
			r.res.Transcript.Cost, r.cfg.Budget.MaxCost))
	default:
		return r.execute(ctx, t, a, s, key)
	}
	return nil
}

func aliasNote(alias string) string {
	if alias == "" {
		return ""
	}
	return "; its result is " + alias
}

// execute runs a tool call that passed every budget.
func (r *run) execute(ctx context.Context, t Tool, a action, s Step, key string) error {
	start := time.Now()
	out, err := t.Run(ctx, a.args)
	s.ElapsedMS = time.Since(start).Milliseconds()
	var abort *AbortError
	switch {
	case errors.As(err, &abort):
		return abort.Err
	case errors.Is(err, ErrInvalidArgs):
		r.refuse(a, s, RejectInvalidArgs, err.Error())
		return nil
	}
	r.res.Transcript.Cost += t.Cost
	s.Cost = t.Cost
	if err != nil {
		s.Status, s.Note = StatusToolError, clip(err.Error(), maxNoteRunes)
		r.record(s)
		r.answer(a, refusalText(a.name, StatusToolError, err.Error()))
		return nil
	}
	alias := r.cite(out.Evidence)
	s.Status, s.Alias = out.Status, alias
	if s.Status == "" {
		s.Status = "ok"
	}
	if alias != "" {
		s.EvidenceID, s.Digest = out.Evidence.ID, out.Evidence.Digest
	}
	r.done[key] = alias
	r.record(s)
	r.answer(a, resultText(a.name, out, alias))
	return nil
}

// cite registers citable evidence under the next alias.
func (r *run) cite(ev *Evidence) string {
	if ev == nil || ev.ID == "" {
		return ""
	}
	alias := fmt.Sprintf("E%d", len(r.aliases)+1)
	r.aliases[alias] = *ev
	return alias
}

func (r *run) refuse(a action, s Step, reason, detail string) {
	r.res.Transcript.Rejected[reason]++
	s.Status, s.Note = StatusRejected, clip(reason+": "+detail, maxNoteRunes)
	r.record(s)
	r.answer(a, refusalText(a.name, reason, detail))
}

// answer sends a call's result: a tool message for a native call, a
// user message for a JSON action.
func (r *run) answer(a action, text string) {
	if a.id != "" {
		r.msgs = append(r.msgs, llm.Message{Role: "tool", ToolCallID: a.id, Content: text})
		return
	}
	r.msgs = append(r.msgs, llm.Message{Role: "user", Content: "Result of " + a.name +
		":\n" + text})
}

// argsOf keeps a call's arguments for the transcript when small.
func argsOf(raw json.RawMessage) json.RawMessage {
	var buf bytes.Buffer
	if json.Compact(&buf, raw) != nil || buf.Len() > maxArgsBytes {
		return nil
	}
	return json.RawMessage(buf.Bytes())
}

// canonical is a call's arguments with sorted keys, so equal calls match.
func canonical(raw json.RawMessage) string {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return string(raw)
	}
	out, err := json.Marshal(v)
	if err != nil {
		return string(raw)
	}
	return string(out)
}
