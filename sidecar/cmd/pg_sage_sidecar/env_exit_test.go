package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// config-08: an unparseable SAGE_* value stops the process with exit code
// 1 before it connects anywhere, naming every such variable.
//
// No concurrent-access test: config loading runs once, before any goroutine.

func TestMainExitsOneOnUnparseableEnv(t *testing.T) {
	// A closed port: if the overlay ignored the junk, the child would fail
	// to connect instead, without naming either variable.
	path := writeChildConfig(t, fmt.Sprintf(
		"mode: standalone\npostgres:\n  host: 127.0.0.1\n  port: %d\n", freePort(t)))
	child := startMainChild(t, []string{"SAGE_PG_PORT=abc", "SAGE_MCP_ENABLED=no"},
		"--config", path)
	if code := child.wait(t, 30*time.Second); code != 1 {
		t.Fatalf("exit code = %d, want 1; output %q", code, child.out.String())
	}
	out := child.out.String()
	for _, want := range []string{
		"[ERROR] [startup] config: ",
		`SAGE_PG_PORT="abc": not an integer`,
		`SAGE_MCP_ENABLED="no": not a boolean`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q lacks %q", out, want)
		}
	}
	if strings.Contains(out, "connected") || strings.Contains(out, "TRUST:") {
		t.Fatalf("process went past config loading: %q", out)
	}
}
