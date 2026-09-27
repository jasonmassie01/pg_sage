package executor

import (
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/sqlast"
)

// executorASTRules hands the executor allowlists to the parse-tree layer.
var executorASTRules = sqlast.Rules{
	SystemParam:     func(name string) bool { return safeAlterSystemParams[strings.ToLower(name)] },
	DatabaseParam:   func(name string) bool { return safeAlterSystemParams[strings.ToLower(name)] },
	ProtectedSchema: isProtectedExecutorSchema,
}

// ASTValidationAvailable reports whether this build carries the
// libpg_query parse-tree layer (cgo builds only).
func ASTValidationAvailable() bool { return sqlast.Available() }

// checkParseTree is the second validation layer: SQL the text rules
// accepted must also have an allowed parse-tree structure.
func checkParseTree(sql string) error {
	if err := sqlast.Check(sql, executorASTRules); err != nil {
		return fmt.Errorf("%w: %v", ErrDisallowedSQL, err)
	}
	return nil
}
