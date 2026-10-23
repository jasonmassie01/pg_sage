// Package auditjob verifies the audit hash chains on a schedule (E2,
// §6.17) and raises a critical finding on a table whose trail fails, so a
// tampered audit is seen in pg_sage, not only by whoever runs the verifier.
package auditjob

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/auditchain"
)

// FindingCategory is the finding a failed verification raises.
const FindingCategory = "audit_chain_broken"

// maxDetailProblems bounds the problems copied into a finding.
const maxDetailProblems = 10

// Source is one database whose chains are verified.
type Source struct {
	Name string
	Pool *pgxpool.Pool
}

// Run verifies every chain installed on src in full, raises a finding per
// failing table and resolves the findings of tables that verify again.
func Run(ctx context.Context, src Source) ([]auditchain.Report, error) {
	if src.Pool == nil {
		return nil, fmt.Errorf("auditjob: %s: no database", src.Name)
	}
	chains, err := auditchain.Installed(ctx, src.Pool)
	if err != nil {
		return nil, fmt.Errorf("auditjob: %s: %w", src.Name, err)
	}
	var reports []auditchain.Report
	var findings []analyzer.Finding
	for _, chain := range chains {
		spec, ok := auditchain.SpecFor(chain)
		if !ok {
			continue
		}
		rep, err := auditchain.Verify(ctx, src.Pool, spec, auditchain.Window{})
		if err != nil {
			return reports, fmt.Errorf("auditjob: %s: %w", src.Name, err)
		}
		reports = append(reports, rep)
		if !rep.OK() {
			findings = append(findings, finding(src.Name, spec, rep))
		}
	}
	if len(chains) == 0 {
		return reports, nil
	}
	return reports, record(ctx, src.Pool, findings)
}

// record upserts the failing tables' findings and resolves the rest.
func record(ctx context.Context, pool *pgxpool.Pool, findings []analyzer.Finding) error {
	if err := analyzer.UpsertFindings(ctx, pool, findings); err != nil {
		return fmt.Errorf("auditjob: raise findings: %w", err)
	}
	active := map[string]bool{}
	for _, f := range findings {
		active[f.ObjectIdentifier] = true
	}
	if err := analyzer.ResolveCleared(ctx, pool, active, FindingCategory); err != nil {
		return fmt.Errorf("auditjob: resolve findings: %w", err)
	}
	return nil
}

func finding(db string, spec auditchain.Spec, rep auditchain.Report) analyzer.Finding {
	problems := rep.Problems
	if len(problems) > maxDetailProblems {
		problems = problems[:maxDetailProblems]
	}
	kinds := map[string]int{}
	for _, p := range rep.Problems {
		kinds[p.Kind]++
	}
	return analyzer.Finding{
		Category: FindingCategory, Severity: "critical", ObjectType: "audit_trail",
		ObjectIdentifier: ObjectIdentifier(spec), DatabaseName: db,
		Title: fmt.Sprintf("The audit trail %s fails verification", spec.Chain),
		Detail: map[string]any{"chain": spec.Chain, "problem_kinds": kinds,
			"problems": problems, "problems_truncated": rep.ProblemsTruncated,
			"head_seq": rep.HeadSeq, "head_hash": rep.HeadHash},
		Recommendation: "Audit rows were edited, deleted or written behind the hash " +
			"chain. Compare the SIEM's copy of the chain (its events carry each link's " +
			"hash) with the database, and find who wrote to the table: the problems " +
			"name the rows and positions.",
	}
}

// ObjectIdentifier names a chain's finding. It is not the table's name:
// pg_sage keeps findings on its own sage.* objects out of every list.
func ObjectIdentifier(spec auditchain.Spec) string { return "audit_trail:" + spec.Chain }
