package main

import (
	"context"

	"github.com/pg-sage/sidecar/internal/earned"
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

// ModelRootAuthority implements sre.RootAuthority.
func (a registryRootAuthority) ModelRootAuthority(ctx context.Context,
	family string) (sre.RootGrant, error) {
	if a.registry == nil {
		return sre.RootGrant{Reason: "no trust ledger for this database, so model roots " +
			"stay advisory (L1)"}, nil
	}
	e, ok := a.registry.Lookup(a.database)
	if !ok || e.Service == nil {
		return sre.RootGrant{Reason: "no trust ledger for this database, so model roots " +
			"stay advisory (L1)"}, nil
	}
	got, err := e.Service.ModelRootAuthority(ctx, earned.Family(family))
	if err != nil {
		return sre.RootGrant{}, err
	}
	return sre.RootGrant{Granted: got.Granted, Reason: got.Reason}, nil
}
