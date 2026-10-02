package executor

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ErrRepairContractInvalid reports a repair contract with a missing or
// inconsistent clause.
var ErrRepairContractInvalid = errors.New("repair contract invalid")

// Reversibility classes of a repair contract (AI-SRE-SPEC §7.2).
const (
	ReversibilityReversible     = "reversible"
	ReversibilityMitigationOnly = "mitigation_only"
	ReversibilityIrreversible   = "irreversible"
)

// BlastRadiusBudget bounds what one action may touch.
type BlastRadiusBudget struct {
	MaxBackends                int `json:"max_backends"`
	MaxDatabases               int `json:"max_databases"`
	MaxActionsPerInvestigation int `json:"max_actions_per_investigation"`
}

// RepairContract extends the typed ActionContract with the clauses every
// Sage SRE action class carries (AI-SRE-SPEC §7.2): fresh-evidence
// preconditions, scope from evidence ids only, lock impact and timeouts,
// the reversibility class, rollback trigger and inverse, post-conditions
// (the recovery predicate), a blast-radius budget and a never-do list.
type RepairContract struct {
	ActionContract
	Version         string
	Preconditions   []string
	Scope           string
	LockImpact      string
	Timeouts        []string
	Reversibility   string
	RollbackTrigger string
	Inverse         string
	PostConditions  []string
	BlastRadius     BlastRadiusBudget
	NeverDo         []string
	ResidualRisk    string
}

// Validate checks every clause is present and consistent with the base
// contract.
func (c RepairContract) Validate() error {
	if err := c.ActionContract.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrRepairContractInvalid, err)
	}
	for name, ok := range map[string]bool{
		"version": c.Version != "", "preconditions": len(c.Preconditions) > 0,
		"scope": strings.TrimSpace(c.Scope) != "", "lock impact": c.LockImpact != "",
		"timeouts": len(c.Timeouts) > 0, "rollback trigger": c.RollbackTrigger != "",
		"inverse": c.Inverse != "", "post-conditions": len(c.PostConditions) > 0,
		"never-do list": len(c.NeverDo) > 0,
		"blast radius": c.BlastRadius.MaxBackends > 0 && c.BlastRadius.MaxDatabases > 0 &&
			c.BlastRadius.MaxActionsPerInvestigation > 0,
	} {
		if !ok {
			return fmt.Errorf("%w: missing %s", ErrRepairContractInvalid, name)
		}
	}
	return c.validateReversibility()
}

func (c RepairContract) validateReversibility() error {
	switch c.Reversibility {
	case ReversibilityReversible, ReversibilityIrreversible:
	case ReversibilityMitigationOnly:
		if c.RollbackClass != ReversibilityMitigationOnly {
			return fmt.Errorf("%w: mitigation-only contract with rollback class %q",
				ErrRepairContractInvalid, c.RollbackClass)
		}
	default:
		return fmt.Errorf("%w: unknown reversibility %q", ErrRepairContractInvalid,
			c.Reversibility)
	}
	return nil
}

// repairContractJSON is the contract's wire form (snake_case, flat).
type repairContractJSON struct {
	ActionType          string            `json:"action_type"`
	RiskTier            string            `json:"risk_tier"`
	RollbackClass       string            `json:"rollback_class"`
	RequiredPermissions []string          `json:"required_permissions"`
	Guardrails          []string          `json:"guardrails"`
	Version             string            `json:"version"`
	Preconditions       []string          `json:"preconditions"`
	Scope               string            `json:"scope"`
	LockImpact          string            `json:"lock_impact"`
	Timeouts            []string          `json:"timeouts"`
	Reversibility       string            `json:"reversibility"`
	RollbackTrigger     string            `json:"rollback_trigger"`
	Inverse             string            `json:"inverse"`
	PostConditions      []string          `json:"post_conditions"`
	BlastRadius         BlastRadiusBudget `json:"blast_radius"`
	NeverDo             []string          `json:"never_do"`
	ResidualRisk        string            `json:"residual_risk"`
}

// MarshalJSON renders the contract flat, in snake_case.
func (c RepairContract) MarshalJSON() ([]byte, error) {
	return json.Marshal(repairContractJSON{ActionType: c.ActionType,
		RiskTier: c.BaseRiskTier, RollbackClass: c.RollbackClass,
		RequiredPermissions: c.RequiredPermissions, Guardrails: c.Guardrails,
		Version: c.Version, Preconditions: c.Preconditions, Scope: c.Scope,
		LockImpact: c.LockImpact, Timeouts: c.Timeouts, Reversibility: c.Reversibility,
		RollbackTrigger: c.RollbackTrigger, Inverse: c.Inverse,
		PostConditions: c.PostConditions, BlastRadius: c.BlastRadius, NeverDo: c.NeverDo,
		ResidualRisk: c.ResidualRisk})
}

// UnmarshalJSON reads the flat form; the base contract's other clauses
// come from the typed contract of the action type.
func (c *RepairContract) UnmarshalJSON(raw []byte) error {
	var w repairContractJSON
	if err := json.Unmarshal(raw, &w); err != nil {
		return err
	}
	base, _ := ContractForActionType(w.ActionType)
	base.ActionType, base.BaseRiskTier, base.RollbackClass = w.ActionType, w.RiskTier,
		w.RollbackClass
	base.RequiredPermissions, base.Guardrails = w.RequiredPermissions, w.Guardrails
	*c = RepairContract{ActionContract: base, Version: w.Version,
		Preconditions: w.Preconditions, Scope: w.Scope, LockImpact: w.LockImpact,
		Timeouts: w.Timeouts, Reversibility: w.Reversibility,
		RollbackTrigger: w.RollbackTrigger, Inverse: w.Inverse,
		PostConditions: w.PostConditions, BlastRadius: w.BlastRadius, NeverDo: w.NeverDo,
		ResidualRisk: w.ResidualRisk}
	return nil
}

// CancelBackendRepairContract is the repair contract of the Sage SRE
// evidence-matched cancel: one backend, always approved, mitigation only.
func CancelBackendRepairContract() RepairContract {
	return RepairContract{
		ActionContract: incidentCancelBackendContract(),
		Version:        "cancel_backend/v1",
		Preconditions: []string{
			"the investigation concluded with one active root blocker that has waiters",
			"identity evidence (pid, backend_start, query_start, database, user, " +
				"query hash, query_id) at most 5 s old when the signal is sent",
			"the identity is rechecked in the signalling statement itself",
			"the target is a client backend of this database, not a replication " +
				"role, pg_sage, a dump/backup tool or an operator-protected backend",
			"a human approved this exact item in the approval queue",
		},
		Scope: "one backend in one database, derived only from the investigation's " +
			"cited evidence ids",
		LockImpact: "takes no locks; the cancelled statement releases its locks unless " +
			"its transaction stays open (idle in transaction, aborted)",
		Timeouts: []string{"approval expires after sre.actions.approval_ttl_minutes",
			"policy lock ceiling and statement timeout of the executor"},
		Reversibility:   ReversibilityMitigationOnly,
		RollbackTrigger: "recovery verification reports not_recovered or inconclusive",
		Inverse: "none: a cancelled statement cannot be resumed; the application may " +
			"retry it, and business effects need owner assessment",
		PostConditions: []string{
			"recovery predicate: the target's wait edges clear in every fresh sample",
			"lock waits fall below the baseline (the backlog drains for connection pressure)",
			"some session keeps making progress (vanished traffic is never recovery)",
		},
		BlastRadius: BlastRadiusBudget{MaxBackends: 1, MaxDatabases: 1,
			MaxActionsPerInvestigation: 1},
		NeverDo: []string{
			"never pg_terminate_backend (termination is not part of this action)",
			"never signal an idle in transaction session: cancel cannot end it",
			"never signal a backend whose identity changed or could not be rechecked",
			"never run without a fresh human approval of this exact item",
			"never retry a signal whose outcome is uncertain",
		},
		ResidualRisk: "PostgreSQL signals by pid: the recheck narrows but cannot close " +
			"the race with the query finishing between the recheck and the signal",
	}
}
