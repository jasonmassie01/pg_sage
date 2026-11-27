package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/pg-sage/sidecar/internal/specialist"
)

// SpecialistService serves the specialist_* tools from the contract
// service shared with the HTTP handler and the adapters (one set of
// per-identity limits). The caller's identity is the bound principal's,
// on the mcp transport; approve never travels.
type SpecialistService struct{ Service *specialist.Service }

// specialistRef is the read tools' and the remediation request's
// arguments.
type specialistRef struct {
	InvestigationID string `json:"investigation_id"`
	RemediationID   string `json:"remediation_id"`
	Reason          string `json:"reason"`
}

// SpecialistCall implements SpecialistBackend.
func (b *SpecialistService) SpecialistCall(ctx context.Context, tool string,
	caller SpecialistCaller, database string, args json.RawMessage) (any, error) {
	id := specialist.Identity{TokenID: caller.TokenID, Name: caller.Name, Kind: caller.Kind,
		Scopes: caller.Scopes, Databases: caller.Databases, Transport: "mcp"}
	var ref specialistRef
	if tool != "specialist_open_investigation" {
		if err := json.Unmarshal(args, &ref); err != nil {
			return nil, fmt.Errorf("%w: %v", specialist.ErrInvalid, err)
		}
	}
	svc := b.Service
	switch tool {
	case "specialist_open_investigation":
		req, err := specialist.DecodeOpenRequest(bytes.NewReader(args))
		if err != nil {
			return nil, err
		}
		return svc.Open(ctx, id, database, req)
	case "specialist_investigation_status":
		return svc.Status(ctx, id, database, ref.InvestigationID)
	case "specialist_investigation_result":
		return svc.Result(ctx, id, database, ref.InvestigationID)
	case "specialist_investigation_transcript":
		return svc.Transcript(ctx, id, database, ref.InvestigationID)
	case "specialist_request_remediation":
		return svc.RequestRemediation(ctx, id, database, ref.InvestigationID,
			ref.RemediationID, specialist.RemediationRequest{Reason: ref.Reason})
	}
	return nil, fmt.Errorf("%w: unknown specialist tool %q", specialist.ErrInvalid, tool)
}
