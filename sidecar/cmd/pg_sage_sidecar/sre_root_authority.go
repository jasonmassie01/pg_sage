package main

import (
	"context"

	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/sre"
)

// registryRootAuthority answers an investigator's model-root authority
// question (roadmap 2.4) from its database's trust ledger, looked up at
// the moment of the question (the ledger may be bound after the
// investigator starts). A database without a ledger keeps model roots
// advisory.
type registryRootAuthority struct {
	registry *earned.Registry
	database string
}

// ModelRootAuthority implements sre.RootAuthority for an investigator
// running model.
func (a registryRootAuthority) ModelRootAuthority(ctx context.Context,
	family, model string) (sre.RootGrant, error) {
	if a.registry == nil {
		return sre.RootGrant{Reason: "no trust ledger for this database, so model roots " +
			"stay advisory (L1)"}, nil
	}
	e, ok := a.registry.Lookup(a.database)
	if !ok || e.Service == nil {
		return sre.RootGrant{Reason: "no trust ledger for this database, so model roots " +
			"stay advisory (L1)"}, nil
	}
	got, err := e.Service.ModelRootAuthority(ctx, earned.Family(family), model)
	if err != nil {
		return sre.RootGrant{}, err
	}
	return sre.RootGrant{Granted: got.Granted, Reason: got.Reason}, nil
}

// llmModelSource reads the model client runs at each call ("" without a
// client): the ledger's hourly reconcile and model-lift view compare a
// measurement with it, as the investigator's questions do.
func llmModelSource(client *llm.Client) func() string {
	return func() string {
		if client == nil {
			return ""
		}
		return client.Model()
	}
}
