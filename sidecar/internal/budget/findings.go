package budget

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
)

// violationFinding is the budget's info finding when today's ledger row
// records a would-cancel or a would-reject.
func violationFinding(b Budget, history []ShadowDay, now time.Time) (analyzer.Finding, bool) {
	today := now.UTC().Truncate(24 * time.Hour)
	var day ShadowDay
	var weekCancels, weekRejects int64
	for _, d := range history {
		if d.Day.Equal(today) {
			day = d
		}
		weekCancels += int64(d.WouldCancel)
		weekRejects = max(weekRejects, int64(d.WouldReject))
	}
	if day.WouldCancel == 0 && day.WouldReject == 0 {
		return analyzer.Finding{}, false
	}
	rec := fmt.Sprintf("Budget %q (%s mode) would have cancelled %d statement(s) and "+
		"rejected up to %d connection(s) today; the longest matching statement ran %.0f ms. "+
		"Warn mode changes nothing: review sage.budget_shadow before raising the budget "+
		"or promoting a role budget to enforce.", b.Name, b.Mode, day.WouldCancel,
		day.WouldReject, day.WorstMs)
	return analyzer.Finding{
		Category: CategoryViolation, Severity: "info", ObjectType: "workload_budget",
		ObjectIdentifier: b.Name,
		Title:            fmt.Sprintf("Workload budget %s exceeded today", b.Name),
		Detail: map[string]any{"budget": b.Name, "mode": string(b.Mode),
			"match": matchText(b.Match), "would_cancel_today": int64(day.WouldCancel),
			"would_reject_today":      int64(day.WouldReject),
			"protected_cancels_today": int64(day.ProtectedCancels),
			"worst_ms_today":          day.WorstMs, "would_cancel_window": weekCancels,
			"would_reject_window_max": weekRejects, "pool_size": b.PoolSize},
		Recommendation: rec,
	}, true
}

func matchText(m Match) string {
	switch {
	case m.Role != "":
		return "role=" + m.Role
	case m.ApplicationName != "":
		return "application_name=" + m.ApplicationName
	}
	return "tag=" + m.Tag
}

// enforceActions are an enforce-mode role budget's actions whose setting
// is not in place yet. A refused role or connection limit raises none.
func (m *Monitor) enforceActions(ctx context.Context, b Budget,
	ws *collector.WorkloadSessions, history []ShadowDay, now time.Time) ([]analyzer.Finding,
	error) {
	self, err := m.catalog.SelfRole(ctx)
	if err != nil {
		return nil, fmt.Errorf("read pg_sage's role for budget %q: %w", b.Name, err)
	}
	facts, err := m.catalog.RoleFacts(ctx, []string{b.Match.Role})
	if err != nil {
		return nil, fmt.Errorf("read role %s for budget %q: %w", b.Match.Role, b.Name, err)
	}
	fact, ok := facts[b.Match.Role]
	if !ok {
		m.logFn("WARN", "budget %q: role %s does not exist; nothing to enforce", b.Name,
			b.Match.Role)
		return nil, nil
	}
	if err := RefusedRole(b.Match.Role, self, fact.Superuser); err != nil {
		m.logFn("WARN", "budget %q not enforced: %v", b.Name, err)
		return nil, nil
	}
	db, err := m.catalog.CurrentDatabase(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the database name for budget %q: %w", b.Name, err)
	}
	var out []analyzer.Finding
	for _, s := range []struct {
		name string
		ms   int64
	}{{SettingStatementTimeout, b.StatementTimeoutMs},
		{SettingIdleInTx, b.IdleInTransactionTimeoutMs}} {
		if f, ok := timeoutAction(b, db, s.name, s.ms, fact); ok {
			if s.name == SettingStatementTimeout {
				m.addCancelWatch(&f, b, history, now)
			}
			out = append(out, f)
		}
	}
	if f, ok := m.connectionLimitAction(b, fact, ws); ok {
		out = append(out, f)
	}
	return out, nil
}

// timeoutAction is the set_role_timeout action of one setting, unless the
// budget sets none or the role already has it.
func timeoutAction(b Budget, db, name string, ms int64, fact RoleFact) (analyzer.Finding,
	bool) {
	if ms <= 0 {
		return analyzer.Finding{}, false
	}
	var prior *string
	if v, ok := fact.Settings[name]; ok {
		if cur, ok := settingMs(v); ok && cur == ms {
			return analyzer.Finding{}, false
		}
		prior = &v
	}
	role := b.Match.Role
	return analyzer.Finding{
		Category: CategoryEnforce, Severity: "info", ObjectType: "role",
		ObjectIdentifier: role + "/" + name,
		Title: fmt.Sprintf("Enforce budget %s: %s %s for role %s", b.Name, name,
			msText(ms), role),
		Detail: map[string]any{"budget": b.Name, "role": role, "database": db,
			"setting": name, "value_ms": ms, "prior": prior,
			"action_type": "set_role_timeout"},
		Recommendation: fmt.Sprintf("Budget %q passed 7 days of warn-mode shadow data with "+
			"no protected application cancelled. Setting %s for role %s in database %s "+
			"makes PostgreSQL enforce it for new sessions; rollback restores the prior "+
			"setting.", b.Name, name, role, db),
		RecommendedSQL: RoleSettingSQL(role, db, name, ms),
		RollbackSQL:    RoleSettingRollbackSQL(role, db, name, prior),
		ActionRisk:     "moderate",
	}, true
}

// connectionLimitAction is the HIGH-risk set_role_connection_limit
// action, refused below the role's current sessions plus a margin.
func (m *Monitor) connectionLimitAction(b Budget, fact RoleFact,
	ws *collector.WorkloadSessions) (analyzer.Finding, bool) {
	if b.ConnectionLimit <= 0 || fact.ConnLimit == b.ConnectionLimit {
		return analyzer.Finding{}, false
	}
	current := 0
	for _, r := range ws.Roles {
		if r.Role == b.Match.Role {
			current = r.ClusterSessions
		}
	}
	if err := CheckConnectionLimit(b.ConnectionLimit, current); err != nil {
		m.logFn("WARN", "budget %q connection limit not enforced: %v", b.Name, err)
		return analyzer.Finding{}, false
	}
	role := b.Match.Role
	return analyzer.Finding{
		Category: CategoryEnforce, Severity: "info", ObjectType: "role",
		ObjectIdentifier: role + "/connection_limit",
		Title: fmt.Sprintf("Enforce budget %s: connection limit %d for role %s", b.Name,
			b.ConnectionLimit, role),
		Detail: map[string]any{"budget": b.Name, "role": role,
			"connection_limit":       b.ConnectionLimit,
			"prior_connection_limit": fact.ConnLimit, "current_sessions": current,
			"action_type": "set_role_connection_limit"},
		Recommendation: fmt.Sprintf("A connection limit is cluster-wide and rejects new "+
			"sessions of role %s past %d (it holds %d now). HIGH risk: it always needs "+
			"approval, and only the fleet leader applies it.", role, b.ConnectionLimit, current),
		RecommendedSQL: ConnectionLimitSQL(role, b.ConnectionLimit),
		RollbackSQL:    ConnectionLimitSQL(role, fact.ConnLimit),
		ActionRisk:     "high_risk",
	}, true
}

func msText(ms int64) string { return fmt.Sprintf("%dms", ms) }

// proposals are the statement_timeout proposals for well-measured roles
// without a role budget that may be budgeted, at most maxProposals.
func (m *Monitor) proposals(ctx context.Context, budgets []Budget,
	now time.Time, enabled bool) ([]analyzer.Finding, error) {
	if !enabled {
		return nil, nil
	}
	evidence, err := m.ledger.RoleEvidence(ctx, now.Add(-proposalWindow))
	if err != nil {
		return nil, fmt.Errorf("read per-role statement time: %w", err)
	}
	candidates := proposalCandidates(budgets, evidence)
	if len(candidates) == 0 {
		return nil, nil
	}
	self, err := m.catalog.SelfRole(ctx)
	if err != nil {
		return nil, fmt.Errorf("read pg_sage's role for proposals: %w", err)
	}
	facts, err := m.catalog.RoleFacts(ctx, candidates)
	if err != nil {
		return nil, fmt.Errorf("read roles for proposals: %w", err)
	}
	var out []analyzer.Finding
	for _, role := range candidates {
		fact, ok := facts[role]
		if !ok || RefusedRole(role, self, fact.Superuser) != nil {
			continue
		}
		out = append(out, proposalFinding(role, evidence[role]))
	}
	return out, nil
}

// proposalCandidates are the measured roles without a role budget,
// sorted, at most maxProposals.
func proposalCandidates(budgets []Budget, evidence map[string]RoleEvidence) []string {
	budgeted := map[string]bool{}
	for _, b := range budgets {
		budgeted[b.Match.Role] = true
	}
	var out []string
	for role, ev := range evidence {
		if budgeted[role] || ev.Calls < proposalMinCalls || ev.P99Ms <= 0 ||
			RefusedRole(role, "", false) != nil {
			continue
		}
		out = append(out, role)
	}
	sort.Strings(out)
	if len(out) > maxProposals {
		out = out[:maxProposals]
	}
	return out
}

func proposalFinding(role string, ev RoleEvidence) analyzer.Finding {
	ms := ProposeStatementTimeoutMs(ev.P99Ms, ev.MaxMs)
	snippet := fmt.Sprintf("budgets:\n  - name: %s\n    match: {role: %s}\n"+
		"    statement_timeout: %dms\n    mode: warn", role, role, ms)
	return analyzer.Finding{
		Category: CategoryProposal, Severity: "info", ObjectType: "role",
		ObjectIdentifier: role,
		Title: fmt.Sprintf("Workload budget proposed for role %s: statement_timeout %dms",
			role, ms),
		Detail: map[string]any{"role": role, "p99_estimate_ms": ev.P99Ms,
			"max_exec_ms": ev.MaxMs, "calls": ev.Calls, "proposed_statement_timeout_ms": ms},
		Recommendation: fmt.Sprintf("Role %s's estimated p99 statement time over the last "+
			"14 days is %.0f ms (longest %.0f ms). A budget of 3x the p99, rounded up and "+
			"never below the longest statement, starts in warn mode and changes nothing:\n%s",
			role, ev.P99Ms, ev.MaxMs, snippet),
	}
}
