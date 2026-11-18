package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/pg-sage/sidecar/internal/evidence/workpack"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/mcp"
)

// workloadPackMCPBackend serves get_workload_pack (evid-02c): the stored
// pack of the resolved database, as its model tier only. Identifiers are
// not hashed (HashIdentifiers stays off until decision D3), and the
// pseudonym map is never returned.
type workloadPackMCPBackend struct{ manager *fleet.DatabaseManager }

func (b workloadPackMCPBackend) WorkloadPackModel(ctx context.Context,
	req mcp.WorkloadPackRequest) (any, error) {
	var inst *fleet.DatabaseInstance
	if b.manager != nil {
		inst = b.manager.GetInstance(req.Database)
	}
	if inst == nil || inst.Pool == nil {
		return nil, fmt.Errorf("%w: database %q is not connected", mcp.ErrWorkloadPackNotFound,
			req.Database)
	}
	_, pack, err := workpack.Load(ctx, inst.Pool, req.ContentSHA256)
	if errors.Is(err, workpack.ErrNotFound) {
		return nil, fmt.Errorf("%w: %s on %s", mcp.ErrWorkloadPackNotFound,
			req.ContentSHA256, req.Database)
	}
	if err != nil {
		return nil, fmt.Errorf("load workload pack %s of %s: %w", req.ContentSHA256,
			req.Database, err)
	}
	model, err := workpack.ForModel(pack, workpack.ModelOptions{})
	if errors.Is(err, workpack.ErrModelLeak) {
		return nil, fmt.Errorf("%w: %w", mcp.ErrWorkloadPackRefused, err)
	}
	if err != nil {
		return nil, fmt.Errorf("derive the model tier of %s: %w", req.ContentSHA256, err)
	}
	return map[string]any{"database": req.Database, "model_tier": workpack.ViewOf(model)}, nil
}
