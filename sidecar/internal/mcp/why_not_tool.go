package mcp

import (
	"context"
	"encoding/json"
	"errors"
)

// why_not (ux-08b) is ux-08a's chain for an agent: why pg_sage has not
// done something on the resolved database, as ten links from observing it
// to the emergency stop, each with one plain sentence and at most one
// typed fix. The same JSON as GET /api/v1/whynot. Read-only: it records no
// decision, takes no budget slot and never returns the change's SQL.

const toolWhyNot = "why_not"

// WhyNotRequest is the question: one finding, or an object and/or a
// category, or (none given) the whole database.
type WhyNotRequest struct {
	// Database is the resolved database the server injects.
	Database  string `json:"database,omitempty"`
	FindingID int64  `json:"finding_id,omitempty"`
	Object    string `json:"object,omitempty"`
	Category  string `json:"category,omitempty"`
}

// WhyNotBackend serves why_not for the resolved database.
type WhyNotBackend interface {
	WhyNot(context.Context, WhyNotRequest) (any, error)
}

// Errors a WhyNotBackend returns: an argument the route also refuses
// (wrapped with the reason), a database that is not monitored or has no
// connection, and a finding that is not in the database.
var (
	ErrWhyNotInvalidArguments = errors.New("invalid arguments")
	ErrWhyNotDatabaseNotFound = errors.New("database not found")
	ErrWhyNotFindingNotFound  = errors.New("finding not found")
)

const whyNotDescription = "Use when a change you expected has not happened, or nothing " +
	"was proposed for a database, finding, object or category. Returns ten checks from " +
	"observed to emergency stop, each pass, fail, unknown or not applicable, with one " +
	"sentence and at most one fix (a GRANT, a setting, a page or a time); the first " +
	"failure is marked. Never changes anything; unknown is never a pass"

func whyNotTools() []Tool {
	return []Tool{{Name: toolWhyNot, Description: whyNotDescription,
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"finding_id":{"type":"integer","minimum":1,"description":"one finding; ` +
			`give it alone"},` +
			`"object":{"type":"string","maxLength":127,"description":"an unquoted ` +
			`schema.name"},` +
			`"category":{"type":"string","maxLength":64,"description":"a finding ` +
			`category, such as unused_index"}},` +
			`"additionalProperties":false}`)}}
}

func (s *Server) callWhyNotTool(ctx context.Context, _ string,
	raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Database  string `json:"database"`
		FindingID *int64 `json:"finding_id"`
		Object    string `json:"object"`
		Category  string `json:"category"`
	}
	if !decodeStrict(raw, &args) {
		return nil, failure(codeInvalidParams, "invalid arguments: why_not takes only "+
			"database, finding_id (an integer), object and category")
	}
	req := WhyNotRequest{Database: args.Database, Object: args.Object,
		Category: args.Category}
	if bound, ok := DatabaseFromContext(ctx); ok {
		req.Database = bound
	}
	if failed := checkWhyNotArgs(args.FindingID, &req); failed != nil {
		return nil, failed
	}
	backend, ok := s.backend.(WhyNotBackend)
	if !ok {
		return nil, failure(codeInternal, "why not unavailable")
	}
	result, err := backend.WhyNot(ctx, req)
	switch {
	case errors.Is(err, ErrWhyNotInvalidArguments):
		return nil, failure(codeInvalidParams, err.Error())
	case errors.Is(err, ErrWhyNotFindingNotFound):
		return nil, failure(codeNotFound, "finding not found")
	case errors.Is(err, ErrWhyNotDatabaseNotFound):
		return nil, failure(codeNotFound, "database not found")
	}
	return toolResult(result, err)
}

// checkWhyNotArgs checks what the schema states; the identifier formats
// are the route's and are checked by the backend.
func checkWhyNotArgs(findingID *int64, req *WhyNotRequest) *rpcError {
	invalid := func(why string) *rpcError {
		return failure(codeInvalidParams, "invalid arguments: "+why)
	}
	switch {
	case len(req.Object) > 127:
		return invalid("object is at most 127 characters")
	case len(req.Category) > 64:
		return invalid("category is at most 64 characters")
	case findingID == nil:
		return nil
	case *findingID < 1:
		return invalid("finding_id must be a positive integer")
	case req.Object != "" || req.Category != "":
		return invalid("finding_id names one finding: give it without object or category")
	}
	req.FindingID = *findingID
	return nil
}

// WhyNot serves why_not.
func (backend *ProductionBackend) WhyNot(ctx context.Context,
	request WhyNotRequest) (any, error) {
	if backend.dependencies.WhyNot == nil {
		return nil, ErrProductionDependencyUnavailable
	}
	return backend.dependencies.WhyNot.WhyNot(ctx, request)
}
