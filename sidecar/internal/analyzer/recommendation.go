package analyzer

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/recommendation"
)

func newRecommendationStore(pool *pgxpool.Pool) *recommendation.Store {
	if pool == nil {
		return nil
	}
	return recommendation.NewStore(pool)
}

// WithPolicyVersion installs the reader of the standing-policy version
// recorded on each new revision. Call it before Run.
func (a *Analyzer) WithPolicyVersion(fn func(context.Context) (int64, error)) {
	a.policyVersion = fn
}

// RecommendationProposal maps a finding to the durable recommendation it
// proposes in database.
func RecommendationProposal(database string, f Finding) recommendation.Proposal {
	return recommendation.Proposal{
		DatabaseName: database, Category: f.Category, Target: f.ObjectIdentifier,
		ObjectType: f.ObjectType, Title: f.Title, Severity: f.Severity,
		ActionRisk: f.ActionRisk, Recommendation: f.Recommendation,
		ForwardSQL: f.RecommendedSQL, InverseSQL: f.RollbackSQL, Evidence: f.Detail,
	}
}

// recordRecommendations writes this cycle's actionable findings as
// recommendation revisions (a revision only when the content hash
// changed) and supersedes the candidates of evaluated categories that
// the cycle no longer emits. The executor acts on these durable rows,
// not on the in-memory findings (C07).
func (a *Analyzer) recordRecommendations(
	ctx context.Context, findings []Finding, evaluated map[string]bool,
) {
	if a.recs == nil {
		return
	}
	version := a.currentPolicyVersion(ctx)
	active := make(map[string]map[string]bool, len(evaluated))
	for _, f := range findings {
		if strings.TrimSpace(f.RecommendedSQL) == "" || isSelfMonitoringFinding(f) {
			continue
		}
		p := RecommendationProposal(a.databaseName, f)
		p.PolicyVersion = version
		// Counted as emitted even if the write fails, so a transient error
		// never supersedes a live recommendation.
		if active[f.Category] == nil {
			active[f.Category] = make(map[string]bool)
		}
		active[f.Category][recommendation.IdentityKey(p)] = true
		if _, err := a.recs.Propose(ctx, p); err != nil {
			a.logFn("ERROR", "analyzer: propose recommendation %s/%s: %v",
				f.Category, f.ObjectIdentifier, err)
		}
	}
	for category := range evaluated {
		if _, err := a.recs.SupersedeAbsent(ctx, a.databaseName, category,
			active[category]); err != nil {
			a.logFn("ERROR", "analyzer: supersede absent %s recommendations: %v",
				category, err)
		}
	}
}

// currentPolicyVersion is the standing-policy version, or nil when it is
// unknown (no reader, or the policy store is unavailable).
func (a *Analyzer) currentPolicyVersion(ctx context.Context) *int64 {
	if a.policyVersion == nil {
		return nil
	}
	version, err := a.policyVersion(ctx)
	if err != nil {
		a.logFn("WARN", "analyzer: standing policy version unavailable: %v", err)
		return nil
	}
	return &version
}
