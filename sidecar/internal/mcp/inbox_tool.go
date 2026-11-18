package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/inbox"
)

// get_inbox (ux-01a) is the inbox for a supervisor that is itself an
// agent: the same payload as GET /api/v1/inbox, read-only. It never
// approves anything and an agent decides nothing from it (plan-03b): every
// item is actionable=false and is decided by a person on its own page.

const toolGetInbox = "get_inbox"

// databaseOptionalTools take the standard database argument but, without
// one, read every database the caller may read instead of requiring one.
// A named database is bound and checked like any other tool's.
var databaseOptionalTools = map[string]bool{toolGetInbox: true, toolGetOutcomes: true,
	toolRestoreDrills: true}

// InboxRequest are get_inbox's typed arguments plus the databases the
// caller may see (nil: all).
type InboxRequest struct {
	Database  string   `json:"database,omitempty"`
	Kinds     []string `json:"kinds,omitempty"`
	Limit     int      `json:"limit,omitempty"`
	Databases []string `json:"-"`
}

// InboxBackend serves get_inbox.
type InboxBackend interface {
	Inbox(context.Context, InboxRequest) (any, error)
}

// ErrInboxDatabaseNotFound is a get_inbox database that does not exist or
// that the caller may not read: one answer for both.
var ErrInboxDatabaseNotFound = errors.New("database not found")

func inboxTools() []Tool {
	kinds := make([]string, 0, 7)
	for _, k := range inbox.Kinds() {
		kinds = append(kinds, fmt.Sprintf("%q", k))
	}
	return []Tool{{Name: toolGetInbox, Description: "List every decision waiting on a " +
		"person (change approvals, plans, autonomy promotions, proposed facts, unsigned " +
		"runbooks, agent capability requests, policy proposals). Returns the items with " +
		"counts by kind, newest risk first. Read-only: it never approves, and an agent " +
		"decides nothing from it; a person decides each item on its page (href). Without " +
		"database it reads every database you may read. A source that could not be read " +
		"is named in errors and the total is then a lower bound",
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"kinds":{"type":"array","items":{"type":"string","enum":[` +
			strings.Join(kinds, ",") + `]},"description":"kinds to list (default all)"},` +
			`"limit":{"type":"integer","minimum":1,"maximum":10000,` +
			`"description":"items returned (default 500); counts cover every item"}},` +
			`"additionalProperties":false}`)}}
}

func (s *Server) callInboxTool(ctx context.Context, _ string,
	raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Database string   `json:"database"`
		Kinds    []string `json:"kinds"`
		Limit    *int     `json:"limit"`
	}
	if !decodeStrict(raw, &args) || args.Limit != nil &&
		(*args.Limit < 1 || *args.Limit > inbox.MaxLimit) {
		return nil, failure(codeInvalidParams, "invalid arguments: get_inbox takes only "+
			"database, kinds and limit (an integer from 1 to 10000)")
	}
	req := InboxRequest{Database: args.Database, Kinds: args.Kinds}
	if args.Limit != nil {
		req.Limit = *args.Limit
	}
	if _, err := inbox.KindsOf(req.Kinds); err != nil {
		return nil, failure(codeInvalidParams, "invalid arguments: "+err.Error())
	}
	if req.Database != "" && validateTargetDatabase(req.Database) != nil {
		return nil, failure(codeInvalidParams, "invalid arguments: invalid database name")
	}
	if p, bound := PrincipalFromContext(ctx); bound && p.Databases != nil {
		req.Databases = append([]string{}, p.Databases...)
	}
	backend, ok := s.backend.(InboxBackend)
	if !ok {
		return nil, failure(codeInternal, "inbox unavailable")
	}
	result, err := backend.Inbox(ctx, req)
	if errors.Is(err, ErrInboxDatabaseNotFound) {
		return nil, failure(codeNotFound, "database not found")
	}
	return toolResult(result, err)
}
