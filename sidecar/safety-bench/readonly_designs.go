package safetybench

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/explain"
)

// RODesign is one read-only design under test. Attempt runs sql under the
// design's protection and returns nil if the statement executed, or an
// error describing how it was turned away (classify maps the error to a
// RefusalClass). A design never commits: it rolls back any work so the
// next case starts clean.
type RODesign interface {
	// Name is the design's stable identifier, used as a report column.
	Name() string
	// Attempt runs sql once. owner is the privileged fixture pool.
	Attempt(ctx context.Context, owner *pgxpool.Pool, sql string) error
}

// readOnlyTxnDesign wraps the statement in BEGIN READ ONLY and rolls back.
// It models "a READ ONLY transaction only": no privilege separation, the
// session's own (owning) role, relying solely on transaction_read_only.
type readOnlyTxnDesign struct{}

func (readOnlyTxnDesign) Name() string { return "read_only_txn" }

func (readOnlyTxnDesign) Attempt(ctx context.Context, owner *pgxpool.Pool, sql string) error {
	conn, err := owner.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire: %w", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "BEGIN READ ONLY"); err != nil {
		return fmt.Errorf("begin read only: %w", err)
	}
	defer func() { _, _ = conn.Exec(ctx, "ROLLBACK") }()
	_, execErr := conn.Exec(ctx, sql)
	return execErr
}

// privRoleDesign runs the statement as a privilege-based read-only role via
// SET ROLE. The role (roleName) holds CONNECT, USAGE and SELECT only, so a
// write fails with 42501 regardless of transaction mode. It models the
// design the bypass taxonomy concludes is the one that holds: privileges on
// the database role.
type privRoleDesign struct{ roleName string }

func (d privRoleDesign) Name() string { return "privilege_role" }

func (d privRoleDesign) Attempt(ctx context.Context, owner *pgxpool.Pool, sql string) error {
	conn, err := owner.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire: %w", err)
	}
	defer conn.Release()
	// A transaction lets us SET LOCAL ROLE and roll everything back,
	// including the role change, whatever the statement does.
	if _, err := conn.Exec(ctx, "BEGIN"); err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _, _ = conn.Exec(ctx, "ROLLBACK") }()
	if _, err := conn.Exec(ctx, "SET LOCAL ROLE "+quoteIdent(d.roleName)); err != nil {
		return fmt.Errorf("set role: %w", err)
	}
	_, execErr := conn.Exec(ctx, sql)
	return execErr
}

// explainGuardDesign runs the statement through the shipped Explainer entry
// point the product uses (internal/explain), the same path exercised by the
// MCP explain tool. The guard rejects any non-read statement before it runs
// (validate.go) and declines ANALYZE for anything not proven side-effect
// free (analyze_guard.go), so a write is never executed: a rejected request
// is ClassBeforeExecution, and a statement that is only ever planned (never
// ANALYZEd) likewise never performs its write.
type explainGuardDesign struct {
	cfg   *config.ExplainConfig
	logFn func(string, string, ...any)
}

func newExplainGuardDesign() explainGuardDesign {
	return explainGuardDesign{
		cfg:   &config.ExplainConfig{Enabled: true, TimeoutMs: config.DefaultExplainTimeoutMs},
		logFn: func(string, string, ...any) {},
	}
}

func (explainGuardDesign) Name() string { return "explain_guard" }

func (d explainGuardDesign) Attempt(ctx context.Context, owner *pgxpool.Pool, sql string) error {
	ex := explain.New(owner, d.cfg, d.logFn)
	res, err := ex.Explain(ctx, explain.ExplainRequest{Query: sql})
	if err != nil {
		// The validator (and any invalid-request path) rejected the
		// statement before execution.
		return beforeExecutionError{reason: err.Error()}
	}
	// The statement parsed as a read statement. If ANALYZE was refused, the
	// query was planned only and never executed its effect; the guard held.
	if res.AnalyzeRefused != "" {
		return beforeExecutionError{reason: "ANALYZE refused: " + res.AnalyzeRefused}
	}
	// A plain read with no refusal: the statement "executed" as a read.
	// For a corpus write case this is a design failure (the write ran, or
	// a side-effecting read was ANALYZEd); for a benign read it is correct.
	return nil
}

// Designs returns the three v0 read-only designs in report order. roleName
// is the privilege-based read-only role the fixture created.
func Designs(roleName string) []RODesign {
	return []RODesign{
		readOnlyTxnDesign{},
		privRoleDesign{roleName: roleName},
		newExplainGuardDesign(),
	}
}
