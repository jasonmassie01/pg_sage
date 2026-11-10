package autonomy

import (
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/custodian/freeze"
	"github.com/pg-sage/sidecar/internal/policy"
)

// catalogPlan gathers one scan's red system catalogs into a single
// plan-only escalation: a catalog VACUUM needs MAINTAIN or ownership.
type catalogPlan struct {
	targets  []freeze.CatalogTarget
	deadline *policy.DeadlineContext
}

// add assesses one catalog horizon and keeps it when it is red.
func (p *catalogPlan) add(c *PostgresFreezeCustodian, row freezeRow, rates freezeRateSample) error {
	sample := row.sample
	sample.Database = c.database
	sample.XIDsPerSecond, sample.MultiXactsPerSecond = rates.xid, rates.mxid
	assessment, err := freeze.EvaluateHorizon(time.Now(), sample, c.threshold)
	if err != nil {
		return fmt.Errorf("evaluate catalog freeze horizon %s.%s: %w",
			sample.Schema, sample.Table, err)
	}
	if assessment.Urgency != freeze.UrgencyRed {
		return nil
	}
	p.targets = append(p.targets, freeze.CatalogTarget{Schema: sample.Schema,
		Table: sample.Table, XIDAge: sample.XIDAge, MaxAge: sample.XIDMaxAge})
	if !rates.known {
		return nil // no consumption rate: no deadline to claim
	}
	deadline := freezePolicyDeadline(assessment.Proposal)
	if deadline != nil && (p.deadline == nil || deadline.HardAt.Before(p.deadline.HardAt)) {
		p.deadline = deadline
	}
	return nil
}

// proposal is the scan's catalog plan, if any catalog is red.
func (p *catalogPlan) proposal(database string) (Proposal, bool, error) {
	if len(p.targets) == 0 {
		return Proposal{}, false, nil
	}
	response, err := freeze.CatalogFreezePlan(p.targets)
	if err != nil {
		return Proposal{}, false, fmt.Errorf("plan catalog freeze: %w", err)
	}
	targets := make([]string, 0, len(p.targets))
	for _, t := range p.targets {
		targets = append(targets, t.Schema+"."+t.Table)
	}
	response.Evidence["plan"] = response.Plan
	return Proposal{Database: database, Feature: "freeze", Plan: response.Plan,
		ResponseKind: string(response.Kind), Evidence: response.Evidence,
		TargetObjects: targets, Deadline: p.deadline}, true, nil
}
