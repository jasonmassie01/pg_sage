package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

func TestFleetReloadConcurrentAppliesSerialize(t *testing.T) {
	env := newFleetReloadEnv(t, "ctl", "b")
	base := configController.Desired()
	const writers = 6
	errs := make(chan error, writers)
	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			candidate := config.Clone(base.Config)
			j := databaseIndex(candidate.Databases, "b")
			candidate.Databases[j].MaxConnections = 3 + i + 1
			errs <- applyWatchedConfig(candidate)
		}()
	}
	wg.Wait()
	close(errs)
	applied := 0
	for err := range errs {
		switch {
		case err == nil:
			applied++
		case errors.Is(err, config.ErrGenerationConflict):
		default:
			t.Fatalf("concurrent reload failed unexpectedly: %v", err)
		}
	}
	if applied != 1 {
		t.Fatalf("%d concurrent reloads from one generation applied, want 1", applied)
	}
	assertFleetMatchesActive(t, env)
}

func TestFleetReloadConcurrentRetriesConverge(t *testing.T) {
	env := newFleetReloadEnv(t, "ctl", "b")
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 40 {
				err := reloadDatabase(t, "b", func(db *config.DatabaseConfig) {
					db.MaxConnections = 3 + i
					db.Tags = []string{fmt.Sprintf("writer-%d", i)}
				})
				if err == nil {
					return
				}
				if !errors.Is(err, config.ErrGenerationConflict) {
					t.Errorf("writer %d: %v", i, err)
					return
				}
			}
			t.Errorf("writer %d never applied", i)
		}()
	}
	wg.Wait()
	assertFleetMatchesActive(t, env)
}

// assertFleetMatchesActive checks there is exactly one live runtime per
// active database, configured as the active config says, and no orphaned
// pool still holds connections beyond it.
func assertFleetMatchesActive(t *testing.T, env *fleetReloadEnv) {
	t.Helper()
	active := configController.Active().Config.Databases
	if fleetMgr.InstanceCount() != len(active) {
		t.Fatalf("instances = %d, active databases = %d",
			fleetMgr.InstanceCount(), len(active))
	}
	for _, db := range active {
		inst := fleetMgr.GetInstance(db.Name)
		if inst == nil || inst.Pool == nil ||
			inst.Config.MaxConnections != db.MaxConnections ||
			inst.Pool.Config().MaxConns != int32(db.MaxConnections) {
			t.Fatalf("db %q runtime does not match the active config", db.Name)
		}
		limit := db.MaxConnections
		eventually(t, 10*time.Second, "orphaned pools of "+db.Name+" to close", func() bool {
			return pgSageConnections(t, env.admin, db.Database) <= limit
		})
	}
}

func TestFleetReloadAddRemoveCyclesLeakNothing(t *testing.T) {
	env := newFleetReloadEnv(t, "ctl")
	extra := testdbExtra(t, env, "cycle")
	cycle := func() {
		if err := env.reload(func(c *config.Config) {
			c.Databases = append(c.Databases, extra)
		}); err != nil {
			t.Fatalf("add: %v", err)
		}
		if fleetMgr.GetInstance("cycle") == nil {
			t.Fatal("cycle database not added")
		}
		if err := env.reload(func(c *config.Config) {
			c.Databases = withoutDatabase(c.Databases, "cycle")
		}); err != nil {
			t.Fatalf("remove: %v", err)
		}
	}
	cycle() // warm process-lifetime singletons (log fanouts, budget reset)
	baseline := settledGoroutines(0, 2*time.Second)
	for range 3 {
		cycle()
	}
	if got := settledGoroutines(baseline+2, 15*time.Second); got > baseline+2 {
		t.Fatalf("goroutines grew from %d to %d over 3 add/remove cycles", baseline, got)
	}
	eventually(t, 5*time.Second, "cycle database backends to close", func() bool {
		return pgSageConnections(t, env.admin, extra.Database) == 0
	})
	eventually(t, 5*time.Second, "retired LLM clients to be released", func() bool {
		return registeredLLMClients("cycle") == 0
	})
}

func registeredLLMClients(database string) int {
	llmClients.mu.Lock()
	defer llmClients.mu.Unlock()
	count := 0
	for _, entry := range llmClients.entries {
		if entry.database == database {
			count++
		}
	}
	return count
}

func TestFleetReloadThroughWatchedYAMLFile(t *testing.T) {
	env := newFleetReloadEnv(t, "ctl")
	extra := testdbExtra(t, env, "watched")
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeFleetYAML(t, path, configController.Active().Config.Databases)
	loader := func() (*config.Config, error) {
		return config.Load([]string{"--config", path})
	}
	initial, err := loader()
	if err != nil {
		t.Fatalf("load initial YAML: %v", err)
	}
	if _, err := configController.Apply(context.Background(),
		configController.Desired().Generation, initial); err != nil {
		t.Fatalf("align controller with YAML: %v", err)
	}
	watcher := config.NewAcknowledgedWatcherWithLoader(path, initial, loader, applyWatchedConfig)
	if err := watcher.Start(); err != nil {
		t.Fatalf("start watcher: %v", err)
	}
	t.Cleanup(watcher.Stop)
	ctl := fleetMgr.GetInstance("ctl")
	writeFleetYAML(t, path, append(configController.Active().Config.Databases, extra))
	eventually(t, 30*time.Second, "watched add of database watched", func() bool {
		return fleetMgr.GetInstance("watched") != nil
	})
	if err := os.WriteFile(path, []byte("mode: fleet\ndatabases: [unclosed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	if fleetMgr.GetInstance("watched") == nil || fleetMgr.GetInstance("ctl") != ctl {
		t.Fatal("an unparsable YAML write changed the running fleet")
	}
}

func writeFleetYAML(t *testing.T, path string, dbs []config.DatabaseConfig) {
	t.Helper()
	var b strings.Builder
	b.WriteString("mode: fleet\ntrust:\n  level: observation\nllm:\n  enabled: false\n")
	b.WriteString("collector:\n  interval_seconds: 3600\nanalyzer:\n  interval_seconds: 3600\n")
	b.WriteString("databases:\n")
	for _, db := range dbs {
		fmt.Fprintf(&b, "  - name: %q\n    host: %q\n    port: %d\n    user: %q\n"+
			"    password: %q\n    database: %q\n    sslmode: disable\n"+
			"    max_connections: %d\n", db.Name, db.Host, db.Port, db.User,
			db.Password, db.Database, db.MaxConnections)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write YAML: %v", err)
	}
}
