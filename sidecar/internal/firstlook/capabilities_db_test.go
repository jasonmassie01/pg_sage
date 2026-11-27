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
	if !strings.Contains(steps, "session_preload_libraries = 'pg_hint_plan', 'auto_explain';") {
		t.Fatalf("auto_explain steps = %q", steps)
	}
}

// The server parses the suggested session_preload_libraries value into
// separate libraries. One literal holding "a,b" would load a library named
// "a,b", and every new session would then fail to start.
func TestSessionPreloadStepParsesAsSeparateLibraries(t *testing.T) {
	pool, ctx := livePool(t)
	e := allReady()
	e.AutoExplainLoaded = false
	e.SessionPreloadLibraries = strp("pg_stat_statements, pg_hint_plan")
	caps, _ := ExtensionCapabilities(e, "self-managed")
	step := capability(caps, CapAutoExplain).Steps[0]
	stmt, ok := strings.CutPrefix(step, "As a superuser: ALTER SYSTEM SET ")
	if !ok {
		t.Fatalf("first step = %q, want an ALTER SYSTEM SET", step)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SET LOCAL "+strings.TrimSuffix(stmt, ";")); err != nil {
		t.Fatalf("run %q: %v", stmt, err)
	}
	var got string
	if err := tx.QueryRow(ctx,
		"SELECT current_setting('session_preload_libraries')").Scan(&got); err != nil {
		t.Fatalf("read session_preload_libraries: %v", err)
	}
	if want := "pg_stat_statements, pg_hint_plan, auto_explain"; got != want {
		t.Fatalf("session_preload_libraries = %q, want %q", got, want)
	}
}
