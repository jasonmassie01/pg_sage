package advisor

import (
	"fmt"
	"maps"
	"strings"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/pgconf"
)

// applyConfigAllowlist decides which LLM-proposed configuration changes
// stay executable (G-P0-1). A GUC or storage parameter outside pgconf's
// allowlists becomes an advisory finding without SQL: the LLM's advice is
// kept for the operator, but pg_sage will not run it. Restart-required
// GUCs stay executable only with operator approval, since pg_sage can
// neither restart the server nor verify the change. A multi-key reloption
// SET is split into one finding per key, so each change is atomic and
// has a faithful one-statement rollback. Other SQL passes through.
func applyConfigAllowlist(findings []analyzer.Finding) []analyzer.Finding {
	if len(findings) == 0 {
		return findings // "no findings" stays nil for callers that check it
	}
	out := make([]analyzer.Finding, 0, len(findings))
	for _, f := range findings {
		if stmt, ok := pgconf.ParseAlterSystem(f.RecommendedSQL); ok {
			out = append(out, gateGUCFinding(f, stmt))
			continue
		}
		if stmt, ok := pgconf.ParseAlterTableReloptions(f.RecommendedSQL); ok {
			out = append(out, gateReloptionFinding(f, stmt)...)
			continue
		}
		out = append(out, f)
	}
	return out
}

func gateGUCFinding(f analyzer.Finding, stmt pgconf.SystemStmt) analyzer.Finding {
	if !pgconf.AdvisorGUC(stmt.Name) {
		return advisoryOnly(f, fmt.Sprintf("%s is not on the list of settings "+
			"pg_sage may change; review and apply it manually if you agree", stmt.Name))
	}
	if pgconf.RequiresRestart(stmt.Name) {
		return withApprovalRequired(f, fmt.Sprintf("%s takes effect only after a "+
			"PostgreSQL restart, which pg_sage cannot perform or verify; an operator "+
			"must approve it and plan the restart", stmt.Name))
	}
	return f
}

func gateReloptionFinding(f analyzer.Finding, stmt pgconf.TableStmt) []analyzer.Finding {
	var refused []string
	for _, opt := range stmt.Options {
		if !pgconf.AdvisorReloption(opt.Key) {
			refused = append(refused, opt.Key)
		}
	}
	if len(refused) > 0 {
		note := fmt.Sprintf("storage parameter(s) %s are not on the list pg_sage may "+
			"change", strings.Join(refused, ", "))
		if disablesAutovacuum(stmt) {
			note += "; disabling autovacuum risks bloat and transaction ID wraparound"
		}
		return []analyzer.Finding{advisoryOnly(f, note)}
	}
	if stmt.Reset || len(stmt.Options) == 1 {
		return []analyzer.Finding{f}
	}
	out := make([]analyzer.Finding, 0, len(stmt.Options))
	for _, opt := range stmt.Options {
		one := f
		one.Detail = maps.Clone(f.Detail)
		one.ObjectIdentifier = f.ObjectIdentifier + ":" + opt.Key
		one.Title = fmt.Sprintf("%s: set %s on %s", f.Category, opt.Key, stmt.Table)
		one.RecommendedSQL = fmt.Sprintf("ALTER TABLE %s SET (%s = %s);",
			stmt.Table, opt.Key, opt.Value)
		one.RollbackSQL = ""
		out = append(out, one)
	}
	return out
}

func disablesAutovacuum(stmt pgconf.TableStmt) bool {
	for _, opt := range stmt.Options {
		base, _ := pgconf.ReloptionBaseKey(opt.Key)
		if base == "autovacuum_enabled" &&
			pgconf.CheckExecutableReloption(opt, stmt.Reset) != nil {
			return true
		}
	}
	return false
}

// advisoryOnly keeps the LLM's advice but removes everything executable.
func advisoryOnly(f analyzer.Finding, why string) analyzer.Finding {
	f.RecommendedSQL, f.RollbackSQL, f.ActionRisk = "", "", ""
	f.Severity = "info"
	f.Recommendation = strings.TrimSpace(f.Recommendation + " (Advisory only: " + why + ".)")
	return f
}

// withApprovalRequired marks a finding so the executor queues it for an
// operator instead of running it unattended.
func withApprovalRequired(f analyzer.Finding, why string) analyzer.Finding {
	f.Detail = maps.Clone(f.Detail)
	if f.Detail == nil {
		f.Detail = map[string]any{}
	}
	f.Detail[analyzer.DetailApprovalRequired] = why
	return f
}

// GateConfigFindings is the one set of gates every proposed GUC or
// storage-parameter change passes, whoever proposed it (the advisor's
// sub-advisors or the tuning agent): values outside the documented safe
// range are dropped, settings outside the allowlists become advisory,
// restart-required settings need an operator, shared_buffers must be
// grounded in host memory, and managed services get their own form
// (ALTER SYSTEM becomes ALTER DATABASE; settings they forbid are
// filtered).
func GateConfigFindings(findings []analyzer.Finding, hostMemBytes int64,
	cloudEnv, dbName string, settings []collector.PGSetting) []analyzer.Finding {
	kept := make([]analyzer.Finding, 0, len(findings))
	for _, f := range findings {
		if ok, _ := ValidateConfigSQL(f.RecommendedSQL); ok {
			kept = append(kept, f)
		}
	}
	kept = applyHostMemoryGuard(kept, hostMemBytes)
	kept = applyConfigAllowlist(kept)
	return TransformForCloud(kept, cloudEnv, dbName, settings)
}
