package specialist

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
)

// Transcript serves an investigation's redacted investigator transcript
// (contract revision 1.1.0): the plan, every tool call with its stored
// result and digest, the claims and the outcome, redacted by pg_sage's
// replay rules (identifiers hashed unless specialist.keep_identifiers).
// not_found when the investigator never ran on the investigation.
func (s *Service) Transcript(ctx context.Context, id Identity, database,
	invID string) (TranscriptResponse, error) {
	b, err := s.admit(id, ScopeRead, database, false)
	if err != nil {
		return TranscriptResponse{}, err
	}
	uid, err := parseInvestigationID(invID)
	if err != nil {
		return TranscriptResponse{}, err
	}
	view, err := b.Transcript(ctx, uid, s.keepIdentifiers)
	if err != nil {
		return TranscriptResponse{}, backendErr(err)
	}
	doc, err := transcriptDocument(view)
	if err != nil {
		return TranscriptResponse{}, err
	}
	return TranscriptResponse{ContractVersion: ContractVersion, Database: database,
		InvestigationID: string(uid), Transcript: doc}, nil
}

// transcriptDocument is the transcript as a JSON object, numbers exact.
func transcriptDocument(v any) (map[string]any, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encode the transcript: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("decode the transcript: %w", err)
	}
	return doc, nil
}
