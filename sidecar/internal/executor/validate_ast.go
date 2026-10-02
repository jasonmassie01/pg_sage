package executor

import (
	"fmt"

	"github.com/pg-sage/sidecar/internal/pgconf"
	"github.com/pg-sage/sidecar/internal/sqlast"
)

// executorASTRules hands the executor allowlists to the parse-tree layer.
var executorASTRules = sqlast.Rules{
	SystemParam:     pgconf.ExecutableGUC,
	DatabaseParam:   pgconf.ExecutableGUC,
	ProtectedSchema: isProtectedExecutorSchema,
	Reloption: func(key, value string, reset bool) bool {
		opt := pgconf.Reloption{Key: key, Value: value}
		return pgconf.CheckExecutableReloption(opt, reset) == nil
	},
}

// astValidationAvailable is swapped only by AssumeASTValidationForTests.
var astValidationAvailable = sqlast.Available

// ASTValidationAvailable reports whether this build carries the
// libpg_query parse-tree layer (cgo builds only). Without it the gate sends
// unattended mutations to operator approval.
func ASTValidationAvailable() bool { return astValidationAvailable() }

// AssumeASTValidationForTests makes the gate treat this build as having the
// parse-tree layer, so tests of unattended execution also run in local
// builds without a C compiler. Call it from TestMain only; SQL checks are
// unchanged (Check is still a no-op without cgo). Never call it in
// production code.
func AssumeASTValidationForTests() {
	astValidationAvailable = func() bool { return true }
}

// checkParseTree is the second validation layer: SQL the text rules
// accepted must also have an allowed parse-tree structure.
func checkParseTree(sql string) error {
	if err := sqlast.Check(sql, executorASTRules); err != nil {
		return fmt.Errorf("%w: %v", ErrDisallowedSQL, err)
	}
	return nil
}
