package main

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/pg-sage/sidecar/internal/agenttools"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/mcp"
)

// MCP v2 wiring (roadmap phase 3): the fleet is the MCP server's database
// directory, and the coding-agent tools run on the pool of the database
// the server resolved for the request, never on another one.

// fleetMCPDirectory lists the fleet's databases for the MCP server.
type fleetMCPDirectory struct{ manager *fleet.DatabaseManager }

func (d fleetMCPDirectory) Databases() []mcp.DatabaseRef {
	refs := []mcp.DatabaseRef{}
	if d.manager == nil {
		return refs
	}
	for name, inst := range d.manager.Instances() {
		ref := mcp.DatabaseRef{Name: name}
		if inst != nil {
			ref.ID = int64(inst.DatabaseID)
		}
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Name < refs[j].Name })
	return refs
}

// agentToolOptions are the coding-agent tool settings from the config.
func agentToolOptions(c *config.Config) agenttools.Options {
	opts := agenttools.Options{Log: func(level, format string, args ...any) {
		logStructured(level, "mcp", format, args...)
	}}
	if c != nil {
		opts.Explain = &c.Explain
		opts.VerifyWindow = time.Duration(c.Verify.WindowMinutes) * time.Minute
	}
	return opts
}

// fleetAgentTools serves the coding-agent tools of the resolved database.
type fleetAgentTools struct {
	manager *fleet.DatabaseManager
	options agenttools.Options
}

var _ mcp.AgentToolBackend = fleetAgentTools{}

func (b fleetAgentTools) tools(ctx context.Context) (*agenttools.Tools, error) {
	name, ok := mcp.DatabaseFromContext(ctx)
	if !ok {
		return nil, fmt.Errorf("%w: no database resolved for the request",
			agenttools.ErrInvalid)
	}
	var inst *fleet.DatabaseInstance
	if b.manager != nil {
		inst = b.manager.GetInstance(name)
	}
	if inst == nil {
		return nil, fmt.Errorf("%w: database %q", agenttools.ErrNotFound, name)
	}
	if inst.Pool == nil {
		return nil, fmt.Errorf("%w: database %q has no connection pool",
			agenttools.ErrUnavailable, name)
	}
	return agenttools.New(inst.Pool, b.options), nil
}

func (b fleetAgentTools) TopQueries(ctx context.Context,
	r agenttools.TopQueriesRequest) (agenttools.TopQueriesResult, error) {
	t, err := b.tools(ctx)
	if err != nil {
		return agenttools.TopQueriesResult{}, err
	}
	return t.TopQueries(ctx, r)
}

func (b fleetAgentTools) ExplainQuery(ctx context.Context,
	r agenttools.ExplainRequest) (agenttools.ExplainResult, error) {
	t, err := b.tools(ctx)
	if err != nil {
		return agenttools.ExplainResult{}, err
	}
	return t.ExplainQuery(ctx, r)
}

func (b fleetAgentTools) WhatIfIndex(ctx context.Context,
	r agenttools.WhatIfRequest) (agenttools.WhatIfResult, error) {
	t, err := b.tools(ctx)
	if err != nil {
		return agenttools.WhatIfResult{}, err
	}
	return t.WhatIfIndex(ctx, r)
}

func (b fleetAgentTools) LintMigration(ctx context.Context,
	r agenttools.LintRequest) (agenttools.LintResult, error) {
	t, err := b.tools(ctx)
	if err != nil {
		return agenttools.LintResult{}, err
	}
	return t.LintMigration(ctx, r)
}

func (b fleetAgentTools) QuerySources(ctx context.Context,
	r agenttools.SourcesRequest) (agenttools.SourcesResult, error) {
	t, err := b.tools(ctx)
	if err != nil {
		return agenttools.SourcesResult{}, err
	}
	return t.QuerySources(ctx, r)
}

func (b fleetAgentTools) MarkObject(ctx context.Context, r agenttools.MarkRequest,
	actor string) (agenttools.MarkResult, error) {
	t, err := b.tools(ctx)
	if err != nil {
		return agenttools.MarkResult{}, err
	}
	return t.MarkObject(ctx, r, actor)
}

func (b fleetAgentTools) SourceFixPacket(ctx context.Context,
	findingID int64) (agenttools.Packet, error) {
	t, err := b.tools(ctx)
	if err != nil {
		return agenttools.Packet{}, err
	}
	packet, err := t.SourceFixPacket(ctx, findingID)
	if err != nil {
		return agenttools.Packet{}, err
	}
	packet.Database, _ = mcp.DatabaseFromContext(ctx)
	return packet, nil
}

func (b fleetAgentTools) ReportSourceFix(ctx context.Context, r agenttools.ReportRequest,
	actor string) (agenttools.Report, error) {
	t, err := b.tools(ctx)
	if err != nil {
		return agenttools.Report{}, err
	}
	return t.ReportSourceFix(ctx, r, actor)
}
