package firstlook

import (
	"strings"
	"testing"
)

// readExtensions reads session_preload_libraries from the server, and the
// self-managed auto_explain step then keeps what is already listed.
func TestReadExtensionsReadsSessionPreloadLibraries(t *testing.T) {
	pool, ctx := livePool(t)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx,
		"SET LOCAL session_preload_libraries = 'pg_hint_plan'"); err != nil {
		t.Fatalf("set session_preload_libraries: %v", err)
	}
	e, err := readExtensions(ctx, tx)
	if err != nil {
		t.Fatalf("read extensions: %v", err)
	}
	if e.SessionPreloadLibraries == nil || *e.SessionPreloadLibraries != "pg_hint_plan" {
		t.Fatalf("session preload = %v, want pg_hint_plan", e.SessionPreloadLibraries)
	}
	e.AutoExplainLoaded = false
	caps, _ := ExtensionCapabilities(e, "self-managed")
	steps := strings.Join(capability(caps, CapAutoExplain).Steps, "\n")
	if !strings.Contains(steps, "session_preload_libraries = 'pg_hint_plan,auto_explain';") {
		t.Fatalf("auto_explain steps = %q", steps)
	}
}
