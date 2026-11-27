// Package auditjob verifies the audit hash chains on a schedule (E2,
// §6.17) and raises a critical finding on a table whose trail fails, so a
// tampered audit is seen in pg_sage, not only by whoever runs the verifier.
package auditjob

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/auditchain"
)

// FindingCategory is the finding a failed verification raises.
const FindingCategory = "audit_chain_broken"

// FailedCategory is the warning a chain raises when it could not be
// verified at all (a timeout, a lost connection): its trail is unproven,
// so its open findings stay open.
const FailedCategory = "audit_chain_verification_failed"

// maxDetailProblems bounds the problems copied into a finding.
const maxDetailProblems = 10

// Source is one database whose chains are verified.
type Source struct {
	Name string
	Pool *pgxpool.Pool
	// Cursors reads the SIEM export cursors of this source's chains; nil
	// when export is off, which leaves open regressions as they are.
	Cursors CursorReader
	// FullVerifyHours is audit.full_verify_interval_hours: hours between
	// full verifications, incremental from the newest checkpoint between
	// them. 0 verifies every chain in full on every run.
	FullVerifyHours int
	// Signer, when set, signs each clean head into its chain as a
	// checkpoint link (the evidence-signing key) and verifies the chains'
	// checkpoint links with it.
	Signer *auditchain.CheckpointSigner
	// PruneBelowSignedCheckpoint is audit.prune_below_signed_checkpoint:
	// after a clean full pass, remove the links below the newest signed
	// checkpoint every SIEM sink has received. Needs Signer and Cursors.
	PruneBelowSignedCheckpoint bool
}

// Run verifies every chain installed on src, from its newest checkpoint
// or in full (see FullVerifyHours), and stores a checkpoint for each that
// verifies clean. It raises a finding per failing table and resolves the
// findings of tables that verify again. A chain that cannot be verified
// gets a FailedCategory warning instead and keeps its findings; the other
// chains are still verified and recorded. It then checks each chain
// against its SIEM cursors (regression.go). The error joins every chain's.
func Run(ctx context.Context, src Source) ([]auditchain.Report, error) {
	if src.Pool == nil {
		return nil, fmt.Errorf("auditjob: %s: no database", src.Name)
	}
	chains, err := auditchain.Installed(ctx, src.Pool)
	if err != nil {
		return nil, fmt.Errorf("auditjob: %s: %w", src.Name, err)
	}
	if len(chains) == 0 {
		return nil, nil
	}
	p := verifyAll(ctx, src, chains)
	errs := p.errs
	if err := recordKeeping(ctx, src.Pool, p.broken, FindingCategory, p.unproven); err != nil {
		errs = append(errs, err)
	}
	if err := record(ctx, src.Pool, p.failed, FailedCategory); err != nil {
		errs = append(errs, err)
	}
	if err := checkRegressions(ctx, src, chains); err != nil {
		errs = append(errs, err)
	}
	return p.reports, errors.Join(errs...)
}

// pass is the outcome of verifying every chain of one source.
type pass struct {
	reports        []auditchain.Report
	broken, failed []analyzer.Finding
	unproven       map[string]bool // chains not verified: their findings stay
	errs           []error
}

func verifyAll(ctx context.Context, src Source, chains []string) pass {
	p := pass{unproven: map[string]bool{}}
	for _, chain := range chains {
		spec, ok := auditchain.SpecFor(chain)
		if !ok {
			continue
		}
		rep, err := verifyChain(ctx, src, spec)
		if err != nil {
			err = fmt.Errorf("auditjob: %s: %w", src.Name, err)
			p.errs = append(p.errs, err)
			p.failed = append(p.failed, failedFinding(src.Name, spec, err))
			p.unproven[ObjectIdentifier(spec)] = true
			continue
		}
		p.reports = append(p.reports, rep)
		if !rep.OK() {
			p.broken = append(p.broken, finding(src.Name, spec, rep))
			continue
		}
		if err := auditchain.StoreCheckpoint(ctx, src.Pool, rep); err != nil {
			p.errs = append(p.errs, fmt.Errorf("auditjob: %s: %w", src.Name, err))
			continue
		}
		if err := signAndPrune(ctx, src, spec, rep); err != nil {
			p.errs = append(p.errs, err)
		}
	}
	return p
}

// verifyChain verifies one chain in full when a full pass is due, else
// from its newest checkpoint.
func verifyChain(ctx context.Context, src Source, spec auditchain.Spec) (
	auditchain.Report, error) {
	full, err := auditchain.FullVerifyDue(ctx, src.Pool, spec.Chain, src.FullVerifyHours)
	if err != nil {
		return auditchain.Report{}, err
	}
	return auditchain.VerifyFromCheckpointKeyed(ctx, src.Pool, spec, full, src.trustedKey(),
		src.PruneBelowSignedCheckpoint)
}

// record upserts one category's failing findings and resolves the rest.
func record(ctx context.Context, pool *pgxpool.Pool, findings []analyzer.Finding,
	category string) error {
	return recordKeeping(ctx, pool, findings, category, nil)
}

// recordKeeping is record that also keeps open the findings on the
// identifiers in keep: objects this run could not check.
func recordKeeping(ctx context.Context, pool *pgxpool.Pool, findings []analyzer.Finding,
	category string, keep map[string]bool) error {
	if err := analyzer.UpsertFindings(ctx, pool, findings); err != nil {
		return fmt.Errorf("auditjob: raise %s findings: %w", category, err)
	}
	active := map[string]bool{}
	for ident := range keep {
		active[ident] = true
	}
	for _, f := range findings {
		active[f.ObjectIdentifier] = true
	}
	if err := analyzer.ResolveCleared(ctx, pool, active, category); err != nil {
		return fmt.Errorf("auditjob: resolve %s findings: %w", category, err)
	}
	return nil
}

func failedFinding(db string, spec auditchain.Spec, err error) analyzer.Finding {
	return analyzer.Finding{
		Category: FailedCategory, Severity: "warning", ObjectType: "audit_trail",
		ObjectIdentifier: ObjectIdentifier(spec), DatabaseName: db,
		Title:  fmt.Sprintf("The audit trail %s could not be verified", spec.Chain),
		Detail: map[string]any{"chain": spec.Chain, "error": err.Error()},
		Recommendation: "The scheduled verification of this chain failed before it " +
			"finished (see the error: a statement timeout, a lock held on the audit " +
			"table, a lost connection), so its trail is unproven. Its open findings " +
			"stay open until a verification finishes. Fix the cause, or verify it on " +
			"demand with GET /api/v1/audit/verify?database=<name>&chain=" + spec.Chain +
			"&full=true.",
	}
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
