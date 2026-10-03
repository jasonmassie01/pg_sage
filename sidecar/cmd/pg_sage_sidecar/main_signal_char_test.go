package main

import (
	"fmt"
	"testing"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// A SIGTERM that arrives while the sidecar is still starting (an
// orchestrator stopping a pod right after it reported ready, or a rolling
// deploy) must shut it down gracefully. The handler used to be installed
// only after the API and Prometheus servers were listening, so a signal in
// that window killed the process (exit -1, no "[shutdown] stopped"); CI hit
// it in the meta-mode characterization test.
func TestMainChar_SIGTERMDuringStartupShutsDownGracefully(t *testing.T) {
	dsn := testdb.CreateDatabase(t, "main_char_early_sigterm")
	api, prom := listenAddrs(t)
	path := writeChildConfig(t, fmt.Sprintf(`meta_db: %q
encryption_key: "characterization-test-passphrase-0123456789"
api:
  listen_addr: %q
prometheus:
  listen_addr: %q
llm:
  enabled: false
`, dsn, api, prom))
	child := startMainChild(t, nil, "--config", path)
	// The first line startup logs, right after loading the config.
	child.waitForOutput(t, childStartTimeout, "[startup] pg_sage sidecar v")
	if code := child.terminate(t); code != 0 {
		t.Fatalf("SIGTERM during startup exit = %d, want a graceful 0; output:\n%s", code,
			redactAdminPassword(child.out.String()))
	}
	assertOrderedLog(t, child.out.String(), "received terminated, shutting down",
		"[shutdown] stopped")
}
