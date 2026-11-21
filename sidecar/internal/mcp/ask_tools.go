package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"

	"github.com/pg-sage/sidecar/internal/ask"
)

// Ask Sage over MCP (roadmap phase 3). ask_sage needs the read scope; the
// run may open an investigation or queue a finding for a person's
// approval only when the principal holds the propose scope. There is no
// approve path: an agent token can ask and propose, never approve. A
// read_only question drops the propose right for that question.

// AskRequest are the typed arguments of ask_sage.
type AskRequest struct {
	Database       string `json:"database,omitempty"`
	Question       string `json:"question"`
	ConversationID string `json:"conversation_id,omitempty"`
	ReadOnly       bool   `json:"read_only,omitempty"`
}

// AskBackend answers ask_sage for the resolved database.
type AskBackend interface {
	AskSage(ctx context.Context, database string, c ask.Caller, r ask.Request) (ask.Answer,
		error)
}

var askToolNames = map[string]bool{"ask_sage": true}

func askTools() []Tool {
	return []Tool{{Name: "ask_sage",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"database":{"type":` +
			`"string","description":"fleet database name"},"question":{"type":"string",` +
			`"minLength":1,"maxLength":2000},"conversation_id":{"type":"string",` +
			`"description":"continue a conversation (its id from an earlier answer)"},` +
			`"read_only":{"type":"boolean","description":"answer only: open, queue and ` +
			`propose nothing for this question"}},` +
			`"required":["question"],"additionalProperties":false}`)}}
}

// askCaller is the bound principal as an Ask Sage caller. An unbound
// (library) caller is a program that may only read.
func askCaller(ctx context.Context) ask.Caller {
	p, bound := PrincipalFromContext(ctx)
	if !bound {
		return ask.Caller{Actor: unboundActor}
	}
	return ask.Caller{Actor: ActorFromContext(ctx), MayPropose: p.Has(ScopePropose)}
}

func (s *Server) callAskTool(ctx context.Context, _ string, raw json.RawMessage) (any,
	*rpcError) {
	backend, ok := s.backend.(AskBackend)
	if !ok {
		return nil, failure(codeInternal, "Ask Sage is unavailable")
	}
	var req AskRequest
	if !decodeStrict(raw, &req) || strings.TrimSpace(req.Question) == "" {
		return nil, failure(codeInvalidParams, "invalid arguments: question is required")
	}
	database, _ := DatabaseFromContext(ctx)
	if database == "" {
		database = req.Database
	}
	r := ask.Request{Question: req.Question, ConversationID: req.ConversationID,
		ReadOnly: req.ReadOnly}
	answer, err := backend.AskSage(ctx, database, askCaller(ctx).For(r), r)
	if err != nil {
		return nil, askFailure(err)
	}
	return toolSuccess(answer), nil
}

func askFailure(err error) *rpcError {
	switch {
	case errors.Is(err, ask.ErrInvalid):
		return failure(codeInvalidParams, err.Error())
	case errors.Is(err, ask.ErrNotFound):
		return failure(codeNotFound, "not found")
	case errors.Is(err, ask.ErrDisabled):
		return unconfigured("Ask Sage is disabled (ask.enabled: false)")
	case errors.Is(err, ask.ErrUnavailable):
		return failure(codeUnavailable, "Ask Sage is temporarily unavailable; retry later")
	}
	slog.Error("ask_sage failed", "error", err)
	return failure(codeInternal, "internal error")
}

// AskSage serves ask_sage through ProductionDependencies.Ask.
func (backend *ProductionBackend) AskSage(ctx context.Context, database string,
	c ask.Caller, r ask.Request) (ask.Answer, error) {
	if backend.dependencies.Ask == nil {
		return ask.Answer{}, ErrProductionDependencyUnavailable
	}
	return backend.dependencies.Ask.AskSage(ctx, database, c, r)
}
