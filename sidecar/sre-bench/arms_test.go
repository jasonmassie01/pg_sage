package srebench

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/sre"
)

// Arms are scored side by side (AI-SRE-SPEC §12): the causal graph with
// the LLM off, the LLM-on arm (the product path; listed and "not
// evaluated" until its model turn is wired), "always escalate" and a
// trivial rules-only baseline.

func env(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

func TestLLMConfigFromEnv(t *testing.T) {
	cases := []struct {
		name    string
		vars    map[string]string
		want    LLMConfig
		wantErr string
	}{
		{"nothing set uses the fake model", nil, LLMConfig{Mode: LLMFake}, ""},
		{"endpoint, model and key", map[string]string{EnvLLMURL: " https://llm.example/v1 ",
			EnvLLMModel: "m-1", EnvLLMKey: "sk-secret"}, LLMConfig{Mode: LLMLive,
			URL: "https://llm.example/v1", Model: "m-1", APIKey: "sk-secret"}, ""},
		{"local model without a key", map[string]string{EnvLLMURL: "http://127.0.0.1:8080/v1",
			EnvLLMModel: "local"}, LLMConfig{Mode: LLMLive, URL: "http://127.0.0.1:8080/v1",
			Model: "local"}, ""},
		{"endpoint without a model", map[string]string{EnvLLMURL: "https://llm.example/v1"},
			LLMConfig{}, EnvLLMModel},
		{"model without an endpoint", map[string]string{EnvLLMModel: "m-1"}, LLMConfig{},
			EnvLLMURL},
		{"key without an endpoint", map[string]string{EnvLLMKey: "sk-secret"}, LLMConfig{},
			EnvLLMURL},
		{"not an http endpoint", map[string]string{EnvLLMURL: "ftp://llm.example",
			EnvLLMModel: "m"}, LLMConfig{}, "http"},
		{"credentials in the endpoint", map[string]string{
			EnvLLMURL: "https://user:pw@llm.example/v1", EnvLLMModel: "m"}, LLMConfig{},
			EnvLLMKey},
	}
	for _, c := range cases {
		got, err := LLMConfigFromEnv(env(c.vars))
		if c.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("%s: err = %v, want one naming %q", c.name, err, c.wantErr)
			}
			if err != nil && strings.Contains(err.Error(), "pw") {
				t.Errorf("%s: error leaks the credentials: %v", c.name, err)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%s: got %+v, %v; want %+v", c.name, got, err, c.want)
		}
	}
}

func TestLLMConfig_NeverExposesTheKey(t *testing.T) {
	c := LLMConfig{Mode: LLMLive, URL: "https://llm.example/v1", Model: "m-1",
		APIKey: "sk-secret"}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{c.String(), string(raw)} {
		if strings.Contains(s, "sk-secret") || !strings.Contains(s, "m-1") {
			t.Fatalf("rendering %q leaks the key or omits the model", s)
		}
	}
	if !strings.Contains(c.String(), "llm.example") {
		t.Fatalf("String() %q does not name the endpoint host", c.String())
	}
	if s := (LLMConfig{Mode: LLMFake}).String(); s != "fake model" {
		t.Fatalf("fake String() = %q", s)
	}
}

func TestDefaultConfig_ListsEveryArm(t *testing.T) {
	cfg := DefaultConfig(2, LLMConfig{Mode: LLMFake})
	want := []string{ArmCausalGraph, ArmLLM, ArmAlwaysEscalate, ArmRulesOnly}
	got := cfg.ArmNames()
	if cfg.Repeats != 2 || strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("repeats %d arms %v, want %v", cfg.Repeats, got, want)
	}
	pending := cfg.Pending()
	if len(pending) != 1 || !strings.Contains(pending[ArmLLM], "not wired") ||
		!strings.Contains(pending[ArmLLM], "fake model") {
		t.Fatalf("pending %v", pending)
	}
	gated := cfg.Gated()
	if len(gated) != 2 || gated[0] != ArmCausalGraph || gated[1] != ArmLLM {
		t.Fatalf("gated %v: every live arm is held to the gates", gated)
	}
}

func TestLLMArm_IsNotReadyAndRefusesToRun(t *testing.T) {
	arm := LLMArm{Config: LLMConfig{Mode: LLMLive, URL: "https://x.example/v1", Model: "m",
		APIKey: "sk-secret"}}
	ready, reason := arm.Ready()
	if ready || reason == "" || strings.Contains(reason, "sk-secret") {
		t.Fatalf("ready=%v reason=%q", ready, reason)
	}
	if _, err := arm.Investigate(t.Context(), &Env{}, Scenario{ID: "x"}); err == nil {
		t.Fatal("a not-ready arm investigated")
	}
}

func TestAlwaysEscalate_AbstainsWithoutProbing(t *testing.T) {
	tr := Trace{Outcome: Outcome{State: sre.StateConcluded, Root: "inactive_slot",
		Ranked: []string{"inactive_slot"}, ProbeCount: 7, Measured: true}}
	o := AlwaysEscalate{}.Derive(Scenario{ID: "x", Family: sre.TriggerWAL}, tr)
	if o.State != sre.StateInconclusive || o.Root != "" || len(o.Ranked) != 0 ||
		o.ProbeCount != 0 || o.Measured || len(o.Forbidden) != 0 {
		t.Fatalf("outcome %+v", o)
	}
}
