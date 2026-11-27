package ask

import (
	"context"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
)

// mcp-09: ask_sage is listed only where it can answer: Ask Sage enabled
// (ask.enabled) with an enabled model. These tests need no database:
// Available reads only the service's configuration.

// switchModel is a model that says whether it is switched on.
type switchModel struct{ on bool }

func (m switchModel) ChatWithTools(context.Context, []llm.Message, []llm.ToolSpec,
	llm.ToolOptions) (llm.ToolResult, error) {
	return llm.ToolResult{}, nil
}

func (m switchModel) IsEnabled() bool { return m.on }

// plainModel cannot say whether it is on, so it counts as on.
type plainModel struct{}

func (plainModel) ChatWithTools(context.Context, []llm.Message, []llm.ToolSpec,
	llm.ToolOptions) (llm.ToolResult, error) {
	return llm.ToolResult{}, nil
}

func availabilityService(enabled bool, model any) *Service {
	d := Deps{Database: "db", Config: config.AskConfig{Enabled: enabled}}
	switch m := model.(type) {
	case switchModel:
		d.Model = m
	case plainModel:
		d.Model = m
	}
	return &Service{d: d}
}

func TestServiceAvailable(t *testing.T) {
	cases := []struct {
		name    string
		enabled bool
		model   any
		want    bool
	}{
		{"enabled with an enabled model", true, switchModel{on: true}, true},
		{"enabled with a model that cannot say", true, plainModel{}, true},
		{"enabled with a disabled model", true, switchModel{on: false}, false},
		{"enabled without a model", true, nil, false},
		{"disabled with an enabled model", false, switchModel{on: true}, false},
		{"disabled without a model", false, nil, false},
	}
	for _, c := range cases {
		if got := availabilityService(c.enabled, c.model).Available(); got != c.want {
			t.Errorf("%s: Available = %v, want %v", c.name, got, c.want)
		}
	}
	var none *Service
	if none.Available() {
		t.Fatal("a nil service is available")
	}
}

func TestRegistryAvailable(t *testing.T) {
	r := NewRegistry()
	if r.Available("orders") || r.Available("") {
		t.Fatal("an empty registry has Ask Sage somewhere")
	}
	on := availabilityService(true, switchModel{on: true})
	off := availabilityService(false, switchModel{on: true})
	r.Set("orders", on)
	r.Set("billing", off)
	if !r.Available("orders") {
		t.Fatal("orders runs an enabled Ask Sage")
	}
	if r.Available("billing") {
		t.Fatal("billing's Ask Sage is disabled")
	}
	if r.Available("gone") {
		t.Fatal("a database without a service has Ask Sage")
	}
	if !r.Available("") {
		t.Fatal("unnamed: some database can answer")
	}
	r.Remove("orders", on)
	if r.Available("") {
		t.Fatal("unnamed: no database can answer any more")
	}
	var nilRegistry *Registry
	if nilRegistry.Available("orders") || nilRegistry.Available("") {
		t.Fatal("a nil registry has Ask Sage")
	}
}

// Concurrent access: availability is read while runtimes register and
// leave (the MCP watcher reads it on its own goroutine).
func TestRegistryAvailableWhileServicesChange(t *testing.T) {
	r := NewRegistry()
	on := availabilityService(true, switchModel{on: true})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				r.Set("orders", on)
				r.Remove("orders", on)
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_ = r.Available("orders")
				_ = r.Available("")
			}
		}()
	}
	wg.Wait()
	if r.Available("orders") {
		t.Fatal("every Set was followed by its Remove")
	}
}
