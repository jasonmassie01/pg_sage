package autonomy

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/ledger"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/schemaguard"
)

// DefaultFamilyIdleWindow is how long a clone family's counters must stay
// unchanged before activity since the statistics reset no longer counts
// (the analyzer's unused_index_window_days default).
const DefaultFamilyIdleWindow = 7 * 24 * time.Hour

// SchemaGuardOptions tunes the PostgreSQL schema guard. Zero values take
// the defaults.
type SchemaGuardOptions struct {
	// IdleWindow: see DefaultFamilyIdleWindow.
	IdleWindow time.Duration
	// Now is the clock for the idle window (tests); nil is time.Now.
	Now func() time.Time
}

func (o SchemaGuardOptions) withDefaults() SchemaGuardOptions {
	if o.IdleWindow <= 0 {
		o.IdleWindow = DefaultFamilyIdleWindow
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return o
}

func NewPostgresSchemaGuard(
	pool *pgxpool.Pool, database string, router ProposalRouter,
	recorder *ledger.Service, retentionPipeline RetentionPipeline,
	options SchemaGuardOptions,
) (*schemaguard.Custodian, error) {
	if pool == nil || strings.TrimSpace(database) == "" || router == nil || recorder == nil {
		return nil, fmt.Errorf("PostgreSQL schema guard dependencies are incomplete")
	}
	policyConfig := schemaguard.DefaultPolicy()
	policyConfig.AllowFKIndexApply = true
	policyConfig.AllowRetentionApply = true
	return schemaguard.NewCustodian(
		newFamilyDetector(pool, newPostgresSchemaDetector(pool, options.Now), options),
		postgresSchemaContractSource{pool}, postgresSchemaHistorySource{pool},
		schemaRemediationRouter{
			database: database, router: router,
			verifiedIndexes: verifiedRouter(router),
			rehearsal:       structuralRouter(router),
			retention: &postgresRetentionEnforcer{
				pool: pool, batchLimit: 1000, pipeline: retentionPipeline,
			},
		},
		schemaDecisionRecorder{recorder}, policyConfig,
	), nil
}

type schemaRemediationRouter struct {
	database        string
	router          ProposalRouter
	verifiedIndexes VerifiedIndexRouter
	rehearsal       StructuralRehearsalRouter
	retention       *postgresRetentionEnforcer
}

func (r schemaRemediationRouter) Route(
	ctx context.Context, item schemaguard.Remediation,
) error {
	proposal := Proposal{
		Database: r.database, Feature: string(policy.ChangeFKIndex),
		SQL:           item.Invariant.ProposedSQL,
		TargetObjects: []string{item.Invariant.Target()},
	}
	switch item.Decision.Route {
	case schemaguard.RouteRetention:
		if r.retention == nil {
			return fmt.Errorf("retention enforcement is unavailable")
		}
		return r.retention.Apply(ctx, item)
	case schemaguard.RouteVerifyIndex:
		if r.verifiedIndexes == nil {
			return fmt.Errorf("verified index lifecycle is unavailable")
		}
		return r.verifiedIndexes.RouteVerifiedIndex(
			ctx, proposal, item.Invariant.RollbackSQL, item.Invariant.QueryIDs,
		)
	case schemaguard.RouteCloneRehearsal:
		if r.rehearsal == nil || strings.TrimSpace(proposal.SQL) == "" {
			return nil
		}
		proposal.Feature = string(policy.ChangeOnlineMigration)
		return r.rehearsal.RouteStructuralRehearsal(ctx, proposal)
	case "":
		if strings.TrimSpace(proposal.SQL) == "" {
			return fmt.Errorf("schema remediation SQL is empty")
		}
		return r.router.Route(ctx, proposal)
	}
	return fmt.Errorf("unsupported schema remediation route %q", item.Decision.Route)
}

func verifiedRouter(router ProposalRouter) VerifiedIndexRouter {
	verified, _ := router.(VerifiedIndexRouter)
	return verified
}

func structuralRouter(router ProposalRouter) StructuralRehearsalRouter {
	rehearsal, _ := router.(StructuralRehearsalRouter)
	return rehearsal
}

// schemaDecisionRecorder writes one sage.decision row per changed schema
// decision. invariant_key and decision_hash let the next cycle tell an
// unchanged decision from a change; a clone family's row lists every
// member it covers in target_objects.
type schemaDecisionRecorder struct{ ledger *ledger.Service }

func (r schemaDecisionRecorder) Record(
	ctx context.Context, record schemaguard.DecisionRecord,
) error {
	item := record.Remediation
	_, err := r.ledger.RecordDecision(ctx, ledger.DecisionInput{
		Feature: "schema_guard", Intent: string(item.Invariant.Kind),
		Evidence:    schemaDecisionEvidence(record),
		ProposedSQL: item.Invariant.ProposedSQL,
		Verdict:     schemaLedgerVerdict(item.Decision.Disposition),
		Reason:      schemaDecisionReason(item.Decision),
		RiskTier:    "moderate", PolicyVersion: 1,
		TargetObjects: append([]string(nil), record.Targets...),
	})
	return err
}

func schemaDecisionEvidence(record schemaguard.DecisionRecord) map[string]any {
	item := record.Remediation
	evidence := map[string]any{
		"route": item.Decision.Route, "disposition": item.Decision.Disposition,
		"invariant_key": record.Identity, "decision_hash": record.Hash,
	}
	if item.Invariant.Subject != "" {
		evidence["subject"] = item.Invariant.Subject
	}
	if family := item.Invariant.Family; family != nil {
		state := "live"
		if family.Idle {
			state = "idle"
		}
		evidence["schema_family"] = family.Key
		evidence["family_state"] = state
		evidence["family_reason"] = family.Reason
		evidence["family_size"] = len(family.Members)
		evidence["affected_schema_count"] = len(record.Targets)
	}
	return evidence
}

func schemaDecisionReason(decision schemaguard.Decision) string {
	if strings.TrimSpace(decision.Reason) != "" {
		return decision.Reason
	}
	return "schema remediation planned; standing policy authorization pending"
}

func schemaLedgerVerdict(disposition schemaguard.Disposition) ledger.Verdict {
	if disposition == schemaguard.DispositionPark {
		return ledger.VerdictPark
	}
	return ledger.VerdictObserveOnly
}
