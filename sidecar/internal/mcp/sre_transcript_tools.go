package mcp

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/pg-sage/sidecar/internal/sre"
)

// sre_get_transcript (roadmap 2.1): the tool-calling investigator's
// transcript of one investigation (plan, each tool call with its result
// and digest, citations, outcome), redacted like the replay export:
// identifiers are keyed hashes unless an operator or admin keeps them.
// Read-only, typed arguments; it never starts or executes anything.

const transcriptToolName = "sre_get_transcript"

// TranscriptBackend serves sre_get_transcript.
type TranscriptBackend interface {
	GetTranscript(ctx context.Context, request InvestigationRequest, keep bool) (any, error)
}

func transcriptTool() Tool {
	return Tool{Name: transcriptToolName, Description: "Read the tool-calling " +
		"investigator's transcript of one investigation: plan, each tool call with its " +
		"redacted result and digest, cited claims and the outcome with its authority",
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"database":{"type":"string","description":"fleet database name"},` +
			`"investigation_id":{"type":"string","format":"uuid"},` +
			`"keep_identifiers":{"type":"boolean","description":"operators only: ` +
			`keep identifiers instead of keyed hashes"}},` +
			`"required":["investigation_id"],"additionalProperties":false}`)}
}

func (s *Server) callTranscriptTool(ctx context.Context, raw json.RawMessage) (any,
	*rpcError) {
	backend, ok := s.backend.(TranscriptBackend)
	if !ok {
		return nil, failure(codeInternal, "transcripts unavailable")
	}
	var req InvestigationRequest
	if !decodeStrict(raw, &req) || req.InvestigationID == "" {
		return nil, failure(codeInvalidParams, "invalid arguments")
	}
	// Unredacted identifiers are a person's operator view (approve scope,
	// never an agent), like the replay-case export's.
	if req.KeepIdentifiers && !mayCall(ctx, ScopeApprove) {
		return nil, failure(codeScopeRequired, "approve scope required to keep "+
			"identifiers: an operator or admin role")
	}
	result, err := backend.GetTranscript(ctx, req, req.KeepIdentifiers)
	if errors.Is(err, sre.ErrNoTranscript) {
		return nil, failure(codeNotFound, "no investigator transcript")
	}
	if err != nil {
		return nil, sreFailure(err)
	}
	return toolSuccess(result), nil
}

// GetTranscript serves sre_get_transcript from the investigations
// dependency when it serves transcripts.
func (backend *ProductionBackend) GetTranscript(ctx context.Context,
	request InvestigationRequest, keep bool) (any, error) {
	t, ok := backend.dependencies.Investigations.(TranscriptBackend)
	if !ok {
		return nil, ErrProductionDependencyUnavailable
	}
	return t.GetTranscript(ctx, request, keep)
}
