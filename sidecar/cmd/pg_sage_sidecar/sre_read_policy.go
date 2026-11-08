package main

import (
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

func (rt *databaseRuntime) sreReadPolicy() *probes.ReadPolicy {
	if rt.cfg == nil || rt.spec.Pool == nil {
		return nil
	}
	settings := rt.cfg.SRE.ReadPolicy
	scope := settings.Scope
	if scope == "" || scope == "auto" {
		if rt.provider != "lakebase" && settings.AllowedSources == nil {
			return nil
		}
		scope = "cluster"
		if rt.provider == "lakebase" {
			scope = "database"
		}
	}
	var allowed []probes.ID
	if settings.AllowedSources != nil {
		allowed = []probes.ID{}
		for _, id := range settings.AllowedSources {
			allowed = append(allowed, probes.ID(id))
		}
	}
	c := rt.spec.Pool.Config().ConnConfig
	return &probes.ReadPolicy{Version: 1, Scope: scope, Database: c.Database,
		Binding: probes.PoolBinding(rt.spec.Pool), Allowed: allowed}
}
