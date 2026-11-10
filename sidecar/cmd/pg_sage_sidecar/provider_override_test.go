package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
)

// No concurrent access tests: resolveProviderOverride is a pure function and
// each runtime resolves its provider once, before any worker starts.

func TestResolveProviderOverride(t *testing.T) {
	cases := []struct {
		name, probed, override, want string
		warns                        bool
	}{
		{"no override keeps the probe", "rds", "", "rds", false},
		{"no override keeps unknown", providerUnknown, "", providerUnknown, false},
		{"unknown takes the override", providerUnknown, "cloud-sql", "cloud-sql", false},
		{"unknown takes self-managed", providerUnknown, providerSelfManaged,
			providerSelfManaged, false},
		{"agreement", "aurora", "aurora", "aurora", false},
		{"self-managed cannot override rds", "rds", providerSelfManaged, "rds", true},
		{"self-managed cannot override a hosted name", "neon", providerSelfManaged,
			"neon", true},
		{"disagreeing managed names keep the probe", "rds", "aurora", "rds", true},
		{"a managed override does not replace self-managed", providerSelfManaged, "rds",
			providerSelfManaged, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, warning := resolveProviderOverride(tc.probed, tc.override)
			if got != tc.want {
				t.Fatalf("provider = %q, want %q", got, tc.want)
			}
			if tc.warns != (warning != "") {
				t.Fatalf("warning = %q, want warning %v", warning, tc.warns)
			}
			if tc.warns && (!strings.Contains(warning, tc.probed) ||
				!strings.Contains(warning, tc.override)) {
				t.Fatalf("warning %q must name the probe %q and the override %q",
					warning, tc.probed, tc.override)
			}
		})
	}
}

func stubRuntimeProvider(t *testing.T, provider string, err error) {
	t.Helper()
	old := detectRuntimeProvider
	detectRuntimeProvider = func(context.Context, *pgxpool.Pool) (string, error) {
		return provider, err
	}
	t.Cleanup(func() { detectRuntimeProvider = old })
}

func overrideRuntime(provider string) *databaseRuntime {
	return &databaseRuntime{spec: databaseRuntimeSpec{Scope: "fleet", Name: "orders",
		Config: config.DatabaseConfig{Name: "orders", Provider: provider}}}
}

// Invalid input: provider: self-managed on an RDS-probed host keeps rds and
// warns.
func TestDetectProviderSelfManagedOverrideKeepsRDS(t *testing.T) {
	stubRuntimeProvider(t, "rds", nil)
	rt := overrideRuntime(providerSelfManaged)
	var got string
	logs := captureStderr(t, func() { got = rt.detectProvider(context.Background()) })
	if got != "rds" || rt.probed != "rds" {
		t.Fatalf("provider = %q probed = %q, want rds and rds", got, rt.probed)
	}
	if !strings.Contains(logs, "[WARN]") || !strings.Contains(logs, "self-managed") ||
		!strings.Contains(logs, "keeps rds") {
		t.Fatalf("logs = %q, want a warning that rds is kept over self-managed", logs)
	}
}

// Error propagation: the probes fail, so the operator's narrower answer is
// used; the probe outcome stays recorded as unknown for the re-probe.
func TestDetectProviderUnknownTakesOverride(t *testing.T) {
	stubRuntimeProvider(t, providerUnknown, errors.New("provider probe aurora: timeout"))
	rt := overrideRuntime("cloud-sql")
	var got string
	logs := captureStderr(t, func() { got = rt.detectProvider(context.Background()) })
	if got != "cloud-sql" || rt.probed != providerUnknown {
		t.Fatalf("provider = %q probed = %q, want cloud-sql and unknown", got, rt.probed)
	}
	if !strings.Contains(logs, "timeout") || !strings.Contains(logs, "cloud-sql") {
		t.Fatalf("logs = %q, want the probe error and the configured provider", logs)
	}
}

func TestDetectProviderWithoutOverrideIsTheProbe(t *testing.T) {
	stubRuntimeProvider(t, "alloydb", nil)
	rt := overrideRuntime("")
	if got := rt.detectProvider(context.Background()); got != "alloydb" ||
		rt.probed != "alloydb" {
		t.Fatalf("provider = %q probed = %q, want alloydb twice", got, rt.probed)
	}
}

// State transition: unknown with an override, then the re-probe confirms
// it: nothing to rebuild, and the worker stops.
func TestProviderReprobeConfirmsOverrideWithoutRebuild(t *testing.T) {
	s := &reprobeScript{answers: []probeResult{probeDown(), {"cloud-sql", nil}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.cancel = cancel
	worker := s.worker()
	worker.current = "cloud-sql"
	if got := worker.run(ctx); got != "cloud-sql" {
		t.Fatalf("run = %q, want cloud-sql", got)
	}
	if len(s.rebuilds) != 0 || s.probes != 2 {
		t.Fatalf("rebuilds = %v probes = %d, want none and 2", s.rebuilds, s.probes)
	}
}

// State transition: the re-probe disagrees with the override, so the
// runtime is rebuilt and its successor resolves probe against override.
func TestProviderReprobeDisagreeingWithOverrideRebuilds(t *testing.T) {
	s := &reprobeScript{answers: []probeResult{{"rds", nil}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.cancel = cancel
	worker := s.worker()
	worker.current = providerSelfManaged
	if got := worker.run(ctx); got != "rds" {
		t.Fatalf("run = %q, want rds", got)
	}
	if len(s.rebuilds) != 1 || s.rebuilds[0] != "rds" {
		t.Fatalf("rebuilds = %v, want [rds]", s.rebuilds)
	}
}

// The re-probe follows the probe outcome, not the overridden provider.
func TestStartProviderReprobeFollowsTheProbe(t *testing.T) {
	rt := overrideRuntime("rds")
	rt.provider, rt.probed = "rds", "rds"
	if rt.needsProviderReprobe() {
		t.Fatal("a definite probe must not need a re-probe")
	}
	rt.probed = providerUnknown
	if !rt.needsProviderReprobe() {
		t.Fatal("an unknown probe with an override still needs a re-probe")
	}
}

// A standalone re-probe that disagrees with a configured provider cannot
// rebuild in place; the warning says which provider stays in force.
func TestMarkProviderPendingNamesTheConfiguredProvider(t *testing.T) {
	rt := overrideRuntime("cloud-sql")
	rt.provider, rt.probed = "cloud-sql", providerUnknown
	logs := captureStderr(t, func() { rt.markProviderPending("alloydb") })
	if !strings.Contains(logs, "found alloydb") ||
		!strings.Contains(logs, "configured provider cloud-sql applies") ||
		strings.Contains(logs, "stay blocked") {
		t.Fatalf("logs = %q, want the configured provider named, not a block", logs)
	}
	rt.provider = providerUnknown
	logs = captureStderr(t, func() { rt.markProviderPending("alloydb") })
	if !strings.Contains(logs, "stay blocked") {
		t.Fatalf("logs = %q, want provider-gated actions blocked under unknown", logs)
	}
}
