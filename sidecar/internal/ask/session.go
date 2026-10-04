package ask

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/pg-sage/sidecar/internal/agentloop"
)

// session is one question's run: the caller, the evidence the tools
// produced (by id, for the answer's citations) and the actions taken. A
// run may open at most one investigation and queue at most one proposal.
type session struct {
	s        *Service
	caller   Caller
	mu       sync.Mutex
	evidence map[string]agentloop.Evidence
	actions  []ActionTaken
	proposed bool
	opened   bool
}

func (s *Service) newSession(c Caller) *session {
	return &session{s: s, caller: c, evidence: map[string]agentloop.Evidence{}}
}

// tools is the closed tool set offered to this caller: the read tools
// whose source exists, and the two write tools only for a caller who may
// propose and only when the wiring supplied their path.
func (ss *session) tools() []agentloop.Tool {
	out := ss.recordTools()
	out = append(out, ss.catalogTools()...)
	if ss.s.d.Investigations != nil {
		out = append(out, ss.investigationTools()...)
	}
	if ss.s.d.Trust != nil {
		out = append(out, ss.trustTool())
	}
	if ss.s.d.Queries != nil {
		out = append(out, ss.queriesTool())
	}
	if ss.caller.MayPropose && ss.s.d.Starter != nil {
		out = append(out, ss.openInvestigationTool())
	}
	if ss.caller.MayPropose && ss.s.d.Proposer != nil {
		out = append(out, ss.proposeTool())
	}
	return out
}

// cite builds a citable output and remembers its evidence.
func (ss *session) cite(id, status, text string) agentloop.Output {
	ev := agentloop.Evidence{ID: id, Digest: digestOf(text), Label: id + " " + status,
		Text: text}
	ss.mu.Lock()
	ss.evidence[id] = ev
	ss.mu.Unlock()
	return agentloop.Output{Status: status, Text: text, Evidence: &ev}
}

func (ss *session) record(a ActionTaken) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	ss.actions = append(ss.actions, a)
}

func (ss *session) snapshot() (map[string]agentloop.Evidence, []ActionTaken) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	ev := make(map[string]agentloop.Evidence, len(ss.evidence))
	for k, v := range ss.evidence {
		ev[k] = v
	}
	return ev, append([]ActionTaken{}, ss.actions...)
}

// tool builds one read-only tool of cost 1 with a strict object schema.
func tool(name, description, properties string, required []string,
	run func(context.Context, json.RawMessage) (agentloop.Output, error)) agentloop.Tool {
	req, _ := json.Marshal(required)
	if required == nil {
		req = []byte("[]")
	}
	schema := `{"type":"object","properties":{` + properties + `},"required":` +
		string(req) + `,"additionalProperties":false}`
	return agentloop.Tool{Name: name, Description: description,
		Parameters: json.RawMessage(schema), Cost: 1, Run: run}
}

// decodeArgs decodes a tool's arguments strictly: a JSON object with
// only the declared fields. Anything else is ErrInvalidArgs.
func decodeArgs(raw json.RawMessage, v any) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return fmt.Errorf("%w: arguments must be a JSON object", agentloop.ErrInvalidArgs)
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%w: %v", agentloop.ErrInvalidArgs, err)
	}
	return nil
}

func invalidArgs(format string, args ...any) error {
	return fmt.Errorf("%w: %s", agentloop.ErrInvalidArgs, fmt.Sprintf(format, args...))
}

// limitOf is a list limit: 0 means def, otherwise 1..max.
func limitOf(n *int, def, max int) (int, error) {
	if n == nil {
		return def, nil
	}
	if *n < 1 || *n > max {
		return 0, invalidArgs("limit must be 1-%d", max)
	}
	return *n, nil
}
