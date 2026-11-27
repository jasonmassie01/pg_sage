package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// MCP resources (llm-06b): each monitored database's latest health
// briefing, read-only. Reading one needs the read scope and a database the
// principal may use, as the read tools do.

// codeResourceNotFound is MCP's "Resource not found" error.
const codeResourceNotFound = -32002

// briefingReadTimeout bounds one resources/read; stdio answers it on the
// session's read loop.
const briefingReadTimeout = 10 * time.Second

const (
	briefingURIPrefix   = "pgsage://databases/"
	briefingURISuffix   = "/briefings/latest"
	defaultBriefingURI  = "pgsage://briefings/latest"
	briefingMIMEType    = "application/json"
	briefingDescription = "The latest pg_sage health briefing of this database: its window, " +
		"open findings by severity, actions, content_text and per-channel delivery status."
)

// ErrNoBriefing is a database with no stored briefing; wrap it with the
// reason.
var ErrNoBriefing = errors.New("no briefing yet")

// BriefingBackend serves the briefing resource: the latest stored briefing
// of database as JSON, or an error wrapping ErrNoBriefing.
type BriefingBackend interface {
	LatestBriefing(ctx context.Context, database string) (json.RawMessage, error)
}

// resourcesList lists one latest-briefing resource per database the
// caller may use (one default resource without a fleet directory); a
// caller without the read scope sees none.
func (s *Server) resourcesList(ctx context.Context) map[string]any {
	resources := []map[string]any{}
	if !mayCall(ctx, ScopeRead) {
		return map[string]any{"resources": resources}
	}
	if s.directory == nil {
		resources = append(resources, briefingResource(defaultBriefingURI, ""))
	}
	for _, name := range s.permittedDatabaseNames(ctx) {
		resources = append(resources, briefingResource(briefingURI(name), name))
	}
	return map[string]any{"resources": resources}
}

func briefingURI(database string) string {
	return briefingURIPrefix + database + briefingURISuffix
}

func briefingResource(uri, database string) map[string]any {
	name, title := "latest-briefing", "Latest briefing"
	if database != "" {
		name, title = "latest-briefing-"+database, "Latest briefing: "+database
	}
	return map[string]any{"uri": uri, "name": name, "title": title,
		"description": briefingDescription, "mimeType": briefingMIMEType}
}

// resourcesRead answers resources/read for a briefing URI.
func (s *Server) resourcesRead(ctx context.Context, params json.RawMessage) (map[string]any,
	*rpcError) {
	var request struct {
		URI *string `json:"uri"`
	}
	if json.Unmarshal(params, &request) != nil || request.URI == nil || *request.URI == "" {
		return nil, failure(codeInvalidParams, "invalid params: resources/read needs a uri")
	}
	uri := *request.URI
	name, ok := parseBriefingURI(uri)
	if !ok {
		return nil, failure(codeResourceNotFound, "Resource not found: "+printableName(uri))
	}
	if !mayCall(ctx, ScopeRead) {
		return nil, failure(codeScopeRequired, "read scope required: a signed-in role, or "+
			"a token with the read scope")
	}
	if failed := databasesUnavailable(ctx); failed != nil {
		return nil, failed
	}
	ref, failed := s.resolveDatabase(ctx, name, nil)
	if failed != nil {
		return nil, failed
	}
	ctx, cancel := context.WithTimeout(WithDatabase(ctx, ref.Name), briefingReadTimeout)
	defer cancel()
	text, failed := s.readLatestBriefing(ctx, ref.Name)
	if failed != nil {
		return nil, failed
	}
	return map[string]any{"contents": []map[string]any{{"uri": uri,
		"mimeType": briefingMIMEType, "text": string(text)}}}, nil
}

// parseBriefingURI returns the database a briefing URI names (nil for the
// default URI); false for any other URI.
func parseBriefingURI(uri string) (*string, bool) {
	if uri == defaultBriefingURI {
		return nil, true
	}
	name, ok := strings.CutPrefix(uri, briefingURIPrefix)
	if !ok {
		return nil, false
	}
	name, ok = strings.CutSuffix(name, briefingURISuffix)
	if !ok || name == "" || strings.ContainsAny(name, "/?#") {
		return nil, false
	}
	return &name, true
}

func (s *Server) readLatestBriefing(ctx context.Context, database string) (json.RawMessage,
	*rpcError) {
	backend, ok := s.backend.(BriefingBackend)
	if !ok {
		return nil, failure(codeUnavailable, "briefings are unavailable on this server")
	}
	text, err := backend.LatestBriefing(ctx, database)
	switch {
	case err == nil:
		return text, nil
	case errors.Is(err, ErrNoBriefing):
		return nil, failure(codeResourceNotFound, fmt.Sprintf("database %q: %s",
			printableName(database), err.Error()))
	case errors.Is(err, ErrIntentUnavailable), errors.Is(err, ErrProductionDependencyUnavailable):
		return nil, failure(codeUnavailable, fmt.Sprintf("briefings of database %q are "+
			"unavailable now; retry later", printableName(database)))
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return nil, failure(codeCancelled, "request cancelled")
	}
	slog.Error("mcp: read the latest briefing", "database", database, "error", err)
	return nil, failure(codeInternal, "internal error")
}
