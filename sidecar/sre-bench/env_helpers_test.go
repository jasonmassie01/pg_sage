package srebench

import (
	"context"
	"testing"
)

// NewEnv bootstraps the sage schema on dsn's database and closes it when
// the test ends. It lives in a test file so the sidecar binary, which
// links this package for game days, never links "testing" (benches-11).
func NewEnv(ctx context.Context, t *testing.T, dsn string) *Env {
	t.Helper()
	env, closeEnv, err := OpenEnv(ctx, dsn)
	if err != nil {
		t.Fatalf("bench database: %v", err)
	}
	t.Cleanup(closeEnv)
	return env
}
