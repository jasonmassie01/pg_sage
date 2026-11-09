package sre

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
)

// llm-01: the coordinator asks the root authority with the model its
// client runs at the moment of the question, so a hot reload to another
// model applies to the next investigation: authority earned by one model
// is never lent to another.

// modelBoundAuthority grants only to the model it measured, as the trust
// ledger does, and records the model each question named.
type modelBoundAuthority struct {
	mu       sync.Mutex
	measured string
	asked    []string
}

func (a *modelBoundAuthority) ModelRootAuthority(_ context.Context, _ string,
	model string) (RootGrant, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.asked = append(a.asked, model)
	if model != a.measured {
		return RootGrant{Reason: "measured with " + a.measured + "; the investigator runs " +
			model}, nil
	}
	return RootGrant{Granted: true, Reason: "16/16 held out"}, nil
}

func (a *modelBoundAuthority) models() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.asked...)
}

func reloadCoordinator(model *llm.Client, auth RootAuthority) (*Coordinator, *logLines) {
	logs := &logLines{}
	return &Coordinator{model: model, authority: auth, logFn: logs.logFn}, logs
}

func TestRootAuthority_ReloadToAnotherModelIsAdvisoryNextTime(t *testing.T) {
	client := llm.New(&config.LLMConfig{Enabled: true, Model: "gpt-6",
		Endpoint: "http://127.0.0.1:1/v1"}, nil)
	auth := &modelBoundAuthority{measured: "gpt-6"}
	c, _ := reloadCoordinator(client, auth)
	inv := Investigation{ID: NewUUID()}
	if g := c.rootAuthority(context.Background(), inv, "lock_blocking"); !g.Granted {
		t.Fatalf("before the reload: %+v, want granted", g)
	}
	client.Reconfigure(&config.LLMConfig{Enabled: true, Model: "gpt-6-luna",
		Endpoint: "http://127.0.0.1:1/v1"})
	g := c.rootAuthority(context.Background(), inv, "lock_blocking")
	if g.Granted || !strings.Contains(g.Reason, "gpt-6-luna") {
		t.Fatalf("after the reload: %+v, want advisory naming the new model", g)
	}
	client.Reconfigure(&config.LLMConfig{Enabled: true, Model: "gpt-6",
		Endpoint: "http://127.0.0.1:1/v1"})
	if g := c.rootAuthority(context.Background(), inv, "lock_blocking"); !g.Granted {
		t.Fatalf("back on the measured model: %+v, want granted", g)
	}
	want := []string{"gpt-6", "gpt-6-luna", "gpt-6"}
	if got := auth.models(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("models asked = %v, want %v", got, want)
	}
}

func TestRootAuthority_NoModelClientAsksWithAnUnknownModel(t *testing.T) {
	auth := &modelBoundAuthority{measured: "gpt-6"}
	c, _ := reloadCoordinator(nil, auth)
	g := c.rootAuthority(context.Background(), Investigation{ID: NewUUID()}, "wal_retention")
	if g.Granted {
		t.Fatalf("no model client: %+v, want advisory", g)
	}
	if got := auth.models(); len(got) != 1 || got[0] != "" {
		t.Fatalf("models asked = %q, want one question with no model", got)
	}
}

func TestRootAuthority_ConcurrentQuestionsDuringAReloadNeverMixModels(t *testing.T) {
	client := llm.New(&config.LLMConfig{Enabled: true, Model: "gpt-6"}, nil)
	auth := &modelBoundAuthority{measured: "gpt-6"}
	c, _ := reloadCoordinator(client, auth)
	var wg sync.WaitGroup
	errs := make(chan error, 40)
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i == 20 {
				client.Reconfigure(&config.LLMConfig{Enabled: true, Model: "llama3"})
			}
			g := c.rootAuthority(context.Background(), Investigation{ID: NewUUID()},
				"lock_blocking")
			if g.Granted == strings.Contains(g.Reason, "llama3") {
				errs <- errors.New("grant and reason disagree: " + g.Reason)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	for _, m := range auth.models() {
		if m != "gpt-6" && m != "llama3" {
			t.Fatalf("asked with %q, want one of the configured models", m)
		}
	}
}
