package schemaguard

import (
	"context"
	"fmt"
	"strings"
)

func ClassifyInvariant(kind InvariantKind) (Classification, error) {
	switch kind {
	case InvariantMissingFKIndex:
		return Classification{Class: RemediationVerifiedIndex, AutoRemediable: true,
			RequiresVerification: true}, nil
	case InvariantUnboundedAppend:
		return Classification{Class: RemediationRetention, AutoRemediable: true,
			RequiresPolicyConsent: true}, nil
	case InvariantEverythingText, InvariantTypeTightening:
		return Classification{Class: RemediationStructural, RequiresRehearsal: true}, nil
	default:
		return Classification{}, fmt.Errorf("unknown invariant kind %q", kind)
	}
}

func Plan(ctx context.Context, request Request) (Decision, error) {
	if err := ctx.Err(); err != nil {
		return Decision{Disposition: DispositionPark}, err
	}
	if strings.TrimSpace(request.Invariant.Schema) == "" ||
		strings.TrimSpace(request.Invariant.Table) == "" {
		return Decision{Disposition: DispositionPark}, fmt.Errorf("schema-qualified target is required")
	}
	classification, err := ClassifyInvariant(request.Invariant.Kind)
	if err != nil {
		return Decision{Disposition: DispositionPark}, err
	}
	decision := Decision{Class: classification.Class,
		RequiresVerification: classification.RequiresVerification,
		RequiresRehearsal:    classification.RequiresRehearsal}
	if exempted(request.Contract.Exemptions, request.Invariant.Kind) {
		decision.Disposition, decision.Reason = DispositionPark, "table contract exemption"
		return decision, nil
	}
	if request.Policy.OscillationLimit > 0 &&
		request.History.ExternalReversions >= request.Policy.OscillationLimit {
		decision.Disposition, decision.Reason = DispositionPark, "oscillation limit reached"
		return decision, nil
	}
	switch classification.Class {
	case RemediationVerifiedIndex:
		return planIndex(request, decision), nil
	case RemediationRetention:
		return planRetention(request, decision), nil
	default:
		// Structural: an advisory recommendation, never routed. pg_sage has
		// no online column migration to rehearse or run (migration-09).
		decision.Route, decision.Disposition = RouteRecommendation, DispositionRecommend
		decision.Reason = StructuralReason
		return decision, nil
	}
}

func planIndex(request Request, decision Decision) Decision {
	decision.Route = RouteVerifyIndex
	if !request.Policy.AllowFKIndexApply {
		decision.Disposition = DispositionRecommend
		return decision
	}
	decision.Disposition, decision.AutoApply = DispositionApply, true
	return decision
}

func planRetention(request Request, decision Decision) Decision {
	decision.Route = RouteRetention
	if !request.Contract.AppendOnly || request.Contract.RetentionWindow <= 0 {
		decision.Disposition, decision.Reason = DispositionPark, "retention contract is incomplete"
		return decision
	}
	if reason := retentionColumnParkReason(request); reason != "" {
		decision.Disposition, decision.Reason = DispositionPark, reason
		return decision
	}
	if !request.Policy.AllowRetentionApply {
		decision.Disposition, decision.Reason = DispositionPark, "policy does not authorize retention"
		return decision
	}
	if request.History.SuccessfulRetentionDryRuns == 0 {
		decision.Disposition = DispositionDryRun
		return decision
	}
	decision.Disposition, decision.AutoApply, decision.MayDeleteData =
		DispositionApply, true, true
	return decision
}

// retentionColumnParkReason returns why retention cannot act on the
// owner-declared column, or "" when it can. pg_sage never deletes by an
// inferred column: its suggestion is surfaced for the owner to confirm.
func retentionColumnParkReason(request Request) string {
	declared := request.Contract.RetentionColumn
	suggestion := "none (no created_at or occurred_at timestamptz, timestamp or date column)"
	if request.Invariant.RetentionSuggestion != "" {
		suggestion = request.Invariant.RetentionSuggestion
	}
	remedy := "; suggested: " + suggestion +
		" — re-declare the contract with retention.column to enable deletes"
	switch {
	case declared == "":
		return "retention column not declared" + remedy
	case request.Invariant.RetentionColumn != declared:
		return fmt.Sprintf("declared retention column %q is missing, dropped or not "+
			"timestamptz, timestamp or date", declared) + remedy
	default:
		return ""
	}
}

func exempted(exemptions []InvariantKind, kind InvariantKind) bool {
	for _, exemption := range exemptions {
		if exemption == kind {
			return true
		}
	}
	return false
}
