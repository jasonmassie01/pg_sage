package agenttools

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// indexes-06: a GIN candidate HypoPG cannot model is reported as such,
// not as a failed what-if, and nothing hypothetical is created or logged.
func TestWhatIfIndexGINNamesTheMethod(t *testing.T) {
	f := newFixture(t)
	f.requirePGSS()
	requireHypoPG(t, f)
	items, qid := itemsWorkload(t, f)
	var mu sync.Mutex
	var warnings []string
	logFn := func(level, format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		warnings = append(warnings, level+": "+fmt.Sprintf(format, args...))
	}
	pool := singleConnPool(t, f)
	res, err := New(pool, Options{Log: logFn}).WhatIfIndex(context.Background(),
		WhatIfRequest{DDL: "CREATE INDEX ON " + items + " USING gin (note gin_trgm_ops)",
			QueryIDs: []QueryID{QueryID(qid)}})
	require.NoError(t, err)
	require.True(t, res.Available)
	require.Equal(t, "unverified", res.Verdict)
	require.Equal(t, "HypoPG cannot model gin indexes; operator approval required",
		res.Reason)
	require.Equal(t, 0, res.Measured)
	require.Equal(t, 0, res.Failed)
	require.Equal(t, int64(0), hypotheticalCount(t, f.ctx, pool))
	for _, w := range warnings {
		require.False(t, strings.Contains(w, "what-if evaluation"), "logged: %s", w)
	}
}
