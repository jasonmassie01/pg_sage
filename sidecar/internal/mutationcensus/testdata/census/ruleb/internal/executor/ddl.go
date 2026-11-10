package executor

import "context"

// Inside internal/executor the helpers are the gate's own: not Rule B.
func run(ctx context.Context) error { return ExecStatistics(ctx) }
