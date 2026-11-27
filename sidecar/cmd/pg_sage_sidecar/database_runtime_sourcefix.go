package main

import (
	"context"
	"time"

	"github.com/pg-sage/sidecar/internal/agenttools"
	"github.com/pg-sage/sidecar/internal/ha"
)

// The source-fix verifier (mcp-06) decides due verdicts in the background:
// a coding agent reports a deploy and rarely comes back to ask.
const (
	sourceFixVerifyInterval = 5 * time.Minute
	sourceFixVerifyLimit    = 50
	// sourceFixVerifyTimeout bounds one pass (its measurements of
	// sage.query_store), well under the interval.
	sourceFixVerifyTimeout = 2 * time.Minute
)

// sourceFixDecider decides due source-fix reports (agenttools.Tools).
type sourceFixDecider interface {
	DecideDue(ctx context.Context, limit int) (int, error)
}

// startSourceFixVerifier decides the database's due source-fix verdicts on
// its own pool every sourceFixVerifyInterval, with the verify window of
// verify.window_minutes.
func (rt *databaseRuntime) startSourceFixVerifier() {
	tools := agenttools.New(rt.spec.Pool, agentToolOptions(rt.cfg))
	pass := sourceFixVerifierPass(rt.spec.Name, tools, rt.ha)
	rt.start(func() { every(rt.ctx, sourceFixVerifyInterval, pass) })
	rt.note("source_fix_verifier")
}

// sourceFixVerifierPass is one bounded pass. It writes verdicts to the
// sage schema, so it runs only while the HA snapshot reads a writable
// primary: not on a replica, an unknown role, in safe mode or inside the
// failover cooldown. A nil monitor fails closed.
func sourceFixVerifierPass(name string, d sourceFixDecider, m *ha.Monitor,
) func(context.Context) {
	return func(ctx context.Context) {
		if m == nil || haNotWritable(m) {
			return
		}
		ctx, cancel := context.WithTimeout(ctx, sourceFixVerifyTimeout)
		defer cancel()
		n, err := d.DecideDue(ctx, sourceFixVerifyLimit)
		if err != nil {
			logWarn("mcp", "db %q: decide due source-fix verdicts: %v", name, err)
		}
		if n > 0 {
			logInfo("mcp", "db %q: decided %d source-fix verdict(s)", name, n)
		}
	}
}
