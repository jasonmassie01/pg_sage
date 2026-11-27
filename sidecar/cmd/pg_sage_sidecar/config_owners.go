package main

import (
	"slices"

	"github.com/pg-sage/sidecar/internal/config"
)

// ownersForMode names the reconfiguration owners a mode registers: the trust
// policy, the LLM connection, the LLM redaction switch (llm-09), the
// briefing settings (llm-06) and the alerting policy (alerting-06) in every
// mode, and the YAML fleet's database
// list only in fleet mode without a meta database (meta mode reads its
// databases from sage.databases). Every other field is restart-bound.
func ownersForMode(c *config.Config) []string {
	owners := []string{config.TrustPolicyOwner, config.LLMOwner, config.LLMRedactionOwner,
		config.BriefingOwner, config.AlertingOwner}
	if c != nil && c.IsFleet() && !c.HasMetaDB() {
		owners = append(owners, config.FleetDatabasesOwner)
	}
	return owners
}

// modeRegistersOwner reports whether the running mode registers owner.
func modeRegistersOwner(owner string) bool {
	return slices.Contains(ownersForMode(cfg), owner)
}
