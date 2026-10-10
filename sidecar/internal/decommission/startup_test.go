package decommission

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

type capturedLog struct {
	mu          sync.Mutex
	info, warns []string
}

func (c *capturedLog) logger() Logger {
	return Logger{
		Info: func(f string, a ...any) {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.info = append(c.info, fmt.Sprintf(f, a...))
		},
		Warn: func(f string, a ...any) {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.warns = append(c.warns, fmt.Sprintf(f, a...))
		},
	}
}

func (c *capturedLog) all() string {
	return strings.Join(c.info, "\n") + "\n" + strings.Join(c.warns, "\n")
}

func clearProviderEnv(t *testing.T) {
	t.Helper()
	for _, c := range envCredentials {
		t.Setenv(c.name, "")
	}
}

// §12 step 2: every inventoried item is logged at startup with its
// provider, ids, location, created_at and delete template.
func TestStartup_LogsEveryItemWithItsTemplate(t *testing.T) {
	clearProviderEnv(t)
	pool, ctx := legacyDB(t, "decom_start")
	seedEstate(t, ctx, pool)
	var log capturedLog
	if err := Startup(ctx, pool, "", log.logger()); err != nil {
		t.Fatalf("Startup: %v", err)
	}
	out := log.all()
	for _, want := range []string{
		"8 resources", InventoryPath, "provider_resource:d-live", "aws_rds",
		"pgsage-d-live", "us-east-2", "created_at=", "aws rds delete-db-instance",
		"local_schema:d-local", `DROP SCHEMA "agentdb_local_s" CASCADE;`,
		"neonctl branches delete 'br-from-receipt' --project-id 'proj-9'",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("startup log lacks %q:\n%s", want, out)
		}
	}
	if len(log.warns) == 0 || !strings.Contains(strings.Join(log.warns, "\n"), "§12") {
		t.Fatalf("unacknowledged resources must be warned about, naming §12: %v", log.warns)
	}
	if strings.Contains(out, "inline-secret-value") {
		t.Fatal("startup logged a secret value")
	}
}

func TestStartup_CleanInstallLogsNothingButSetCredentials(t *testing.T) {
	clearProviderEnv(t)
	pool, ctx := freshDB(t, "decom_start_clean")
	var log capturedLog
	if err := Startup(ctx, pool, "", log.logger()); err != nil {
		t.Fatalf("Startup: %v", err)
	}
	if len(log.info)+len(log.warns) != 0 {
		t.Fatalf("a clean install logged: %s", log.all())
	}
	t.Setenv("PG_SAGE_NEON_API_KEY", "neon-key-value")
	if err := Startup(ctx, pool, "", log.logger()); err != nil {
		t.Fatal(err)
	}
	warned := strings.Join(log.warns, "\n")
	if !strings.Contains(warned, "PG_SAGE_NEON_API_KEY") || strings.Contains(warned, "neon-key-value") {
		t.Fatalf("credential warning = %q", warned)
	}
	if len(log.warns) != 1 {
		t.Fatalf("one warning per startup, got %d", len(log.warns))
	}
}

// §12 step 4: the YAML acknowledgement is recorded with the config as actor.
func TestStartup_AppliesTheYAMLAcknowledgement(t *testing.T) {
	clearProviderEnv(t)
	pool, ctx := freshDB(t, "decom_start_yaml")
	bootstrapped(t, ctx, pool)
	applyLegacySchema(t, ctx, pool)
	seedEstate(t, ctx, pool)
	path := writeConfig(t, minimalConfig+"agentdb_decommission:\n"+
		"  acknowledged_resources: [provider_resource:d-op, provider_resource:gone]\n"+
		"  exported: true\n")
	var log capturedLog
	if err := Startup(ctx, pool, path, log.logger()); err != nil {
		t.Fatalf("Startup: %v", err)
	}
	rows := ackRows(t, ctx, pool)
	if len(rows) != 1 || rows[0].id != "provider_resource:d-op" || rows[0].source != "yaml" ||
		!strings.HasPrefix(rows[0].by, "config:") {
		t.Fatalf("yaml ack rows = %+v", rows)
	}
	if !strings.Contains(strings.Join(log.warns, "\n"), "provider_resource:gone") {
		t.Fatalf("an unknown acknowledged id must be warned about: %v", log.warns)
	}
	log = capturedLog{}
	if err := Startup(ctx, pool, path, log.logger()); err != nil {
		t.Fatal(err)
	}
	if got := ackRows(t, ctx, pool); len(got) != 1 {
		t.Fatalf("a restart re-recorded the acknowledgement: %+v", got)
	}
	unexported := writeConfig(t, minimalConfig+"agentdb_decommission:\n"+
		"  acknowledged_resources: [provider_resource:d-live]\n")
	log = capturedLog{}
	if err := Startup(ctx, pool, unexported, log.logger()); err != nil {
		t.Fatal(err)
	}
	if got := ackRows(t, ctx, pool); len(got) != 1 ||
		!strings.Contains(strings.Join(log.warns, "\n"), "exported") {
		t.Fatalf("an unexported yaml ack must be refused with a warning: rows %+v, warns %v",
			got, log.warns)
	}
}

func TestStartup_InventoryFailureIsReturned(t *testing.T) {
	pool, ctx := legacyDB(t, "decom_start_fail")
	pool.Close()
	var log capturedLog
	err := Startup(ctx, pool, "", log.logger())
	if err == nil || !strings.Contains(err.Error(), "inventory") {
		t.Fatalf("Startup on a closed pool = %v", err)
	}
	if err := Startup(ctx, nil, "", log.logger()); err == nil {
		t.Fatal("Startup without a control pool must fail loudly")
	}
}
