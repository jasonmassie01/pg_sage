package llm

import (
	"context"
	"time"

	"github.com/pg-sage/sidecar/internal/selfbudget"
)

// recordWait adds the time since start to the LLM wait of the loop run
// ctx carries (footprint-07): a loop's time waiting on the provider is not
// its local work. Outside a tracked run it does nothing.
func recordWait(ctx context.Context, start time.Time) {
	selfbudget.AddWait(ctx, time.Since(start))
}
