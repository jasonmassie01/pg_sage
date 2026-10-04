package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/facts"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/mcp"
)

// factsMCPBackend serves the MCP fact tools (roadmap 2.3) from each
// database's fact store. A proposal made over MCP is recorded as proposed
// (it binds nothing until a person confirms it); decide_fact records the
// principal as the decider.
type factsMCPBackend struct{ manager *fleet.DatabaseManager }

func (b factsMCPBackend) store(database string) (*facts.Store, string, error) {
	if b.manager == nil {
		return nil, "", fmt.Errorf("%w: no monitored databases", facts.ErrInvalidValue)
	}
	if database == "" {
		names := make([]string, 0)
		for name := range b.manager.Instances() {
			names = append(names, name)
		}
		if len(names) != 1 {
			return nil, "", fmt.Errorf("%w: database is required", facts.ErrInvalidValue)
		}
		database = names[0]
	}
	inst := b.manager.GetInstance(database)
	if inst == nil || inst.Pool == nil {
		return nil, "", fmt.Errorf("%w: unknown database %q", facts.ErrInvalidValue, database)
	}
	return newFactStore(inst.Pool), database, nil
}

func (b factsMCPBackend) ListFacts(ctx context.Context, req mcp.FactRequest) (any, error) {
	s, name, err := b.store(req.Database)
	if err != nil {
		return nil, err
	}
	filter := facts.Filter{Type: facts.Type(req.Type)}
	for _, st := range strings.Split(req.Status, ",") {
		if st = strings.TrimSpace(st); st != "" {
			filter.Status = append(filter.Status, facts.Status(st))
		}
	}
	got, err := s.List(ctx, filter)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(got))
	for _, f := range got {
		out = append(out, map[string]any{"fact": f, "summary": f.Describe(),
			"provenance": f.Provenance()})
	}
	return map[string]any{"database": name, "facts": out}, nil
}

func (b factsMCPBackend) ProposeFact(ctx context.Context, req mcp.FactRequest,
	actor string) (any, error) {
	s, name, err := b.store(req.Database)
	if err != nil {
		return nil, err
	}
	f, created, err := s.Propose(ctx, facts.Proposal{Type: facts.Type(req.Type),
		Kind: facts.Kind(req.SubjectKind), Subject: req.Subject, Value: req.Value,
		Source: facts.SourceOperator, ProposedBy: actor,
		Evidence: []facts.Citation{{Kind: "mcp", Ref: actor, Detail: req.Evidence}}})
	if err != nil {
		return nil, err
	}
	return map[string]any{"database": name, "fact": f, "created": created,
		"summary": f.Describe(), "note": "proposed: it binds nothing until a person " +
			"confirms it (UI, API, a chat card or decide_fact)"}, nil
}

func (b factsMCPBackend) DecideFact(ctx context.Context, req mcp.FactRequest,
	actor string) (any, error) {
	s, name, err := b.store(req.Database)
	if err != nil {
		return nil, err
	}
	f, err := s.Decide(ctx, req.FactID, facts.Decision{Confirm: req.Decision == "confirm",
		Actor: actor, Note: req.Note})
	if err != nil {
		return nil, err
	}
	return map[string]any{"database": name, "fact": f, "summary": f.Describe(),
		"provenance": f.Provenance()}, nil
}
