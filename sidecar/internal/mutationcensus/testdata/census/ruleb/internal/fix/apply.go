package fix

import (
	"context"
	"time"

	exec "github.com/pg-sage/sidecar/internal/executor"
)

// Applier runs dynamic SQL through the raw executor helper: Rule B.
type Applier struct{ pool any }

func (a *Applier) Apply(ctx context.Context, sql string) error {
	return exec.ExecInTransaction(ctx, nil, sql, time.Second)
}

// Pick hands the raw helper on as a value: still Rule B.
func Pick() any { return exec.ExecConcurrently }
