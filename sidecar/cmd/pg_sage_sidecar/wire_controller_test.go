package main

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// testWireRouter wires the router the way startup does: every mode builds a
// config controller first (config-10), so a test that passes none gets one
// seeded from its Cfg. A wiring error panics.
func testWireRouter(p WireParams) WireResult {
	if p.Config == nil {
		p.Config = config.NewConfigController(p.Cfg, nil)
	}
	result, err := wireRouter(p)
	if err != nil {
		panic(err)
	}
	return result
}

func TestWireRouter_WithoutControllerFailsClosed(t *testing.T) {
	result, err := wireRouter(WireParams{Cfg: config.DefaultConfig()})
	if err == nil {
		t.Fatal("wireRouter without a config controller returned no error")
	}
	if !strings.Contains(err.Error(), "config controller") {
		t.Fatalf("error = %q, want it to name the config controller", err)
	}
	if result.Handler != nil {
		t.Fatalf("handler = %T, want none when wiring fails", result.Handler)
	}
}

func TestWireRouter_WithControllerBuildsHandler(t *testing.T) {
	cfg := config.DefaultConfig()
	result, err := wireRouter(WireParams{
		Cfg: cfg, Config: config.NewConfigController(cfg, nil),
	})
	if err != nil {
		t.Fatalf("wireRouter: %v", err)
	}
	if result.Handler == nil {
		t.Fatal("wireRouter returned no handler")
	}
}
