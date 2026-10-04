package srebench

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/sre"
)

// Arms are scored side by side (AI-SRE-SPEC §12): the causal graph with
// the LLM off, the LLM-on arm (the product path: the fake adversarial
// model in CI or a live endpoint), "always escalate" and a trivial
// rules-only baseline.

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
		// Roadmap 2.4: a live endpoint also needs PG_SAGE_LIVE_LLM=1 and the
		// run's caps (llmbudget_test.go covers them); these cases are about
		// the endpoint, so they set both.
		vars := c.vars
		if vars[EnvLLMURL] != "" {
			vars = liveVars(nil)
			delete(vars, EnvLLMKey)
			for k, v := range c.vars {
				vars[k] = v
			}
			if _, ok := c.vars[EnvLLMModel]; !ok {
				delete(vars, EnvLLMModel)
			}
		}
		got, err := LLMConfigFromEnv(env(vars))
		if c.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("%s: err = %v, want one naming %q", c.name, err, c.wantErr)
			}
			if err != nil && strings.Contains(err.Error(), "pw") {
				t.Errorf("%s: error leaks the credentials: %v", c.name, err)
			}
			continue
		}
		if err != nil || got.Mode != c.want.Mode || got.URL != c.want.URL ||
			got.Model != c.want.Model || got.APIKey != c.want.APIKey {
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
	if pending := cfg.Pending(); len(pending) != 0 {
		t.Fatalf("pending %v: the LLM-on arm is wired and ready with the fake model", pending)
	}
	gated := cfg.Gated()
	if len(gated) != 2 || gated[0] != ArmCausalGraph || gated[1] != ArmLLM {
		t.Fatalf("gated %v: every live arm is held to the gates", gated)
	}
}

// The LLM-on arm is ready in fake mode (CI default) and in live mode with
// an endpoint and a model; a live config missing either is not ready.
func TestLLMArm_ReadinessPerMode(t *testing.T) {
	cases := []struct {
		name  string
		cfg   LLMConfig
		ready bool
	}{
		{"fake", LLMConfig{Mode: LLMFake}, true},
		{"live", LLMConfig{Mode: LLMLive, URL: "https://x.example/v1", Model: "m",
			APIKey: "sk-secret"}, true},
		{"live without a key (local model)", LLMConfig{Mode: LLMLive,
			URL: "http://127.0.0.1:8080/v1", Model: "local"}, true},
		{"live without a model", LLMConfig{Mode: LLMLive, URL: "https://x.example/v1",
			APIKey: "sk-secret"}, false},
		{"live without an endpoint", LLMConfig{Mode: LLMLive, Model: "m",
			APIKey: "sk-secret"}, false},
		{"unknown mode", LLMConfig{Mode: "magic"}, false},
	}
	for _, c := range cases {
		ready, reason := LLMArm{Config: c.cfg}.Ready()
		if ready != c.ready || (!ready && reason == "") || strings.Contains(reason, "sk-secret") {
			t.Errorf("%s: ready=%v reason=%q", c.name, ready, reason)
		}
	}
	bad := LLMArm{Config: LLMConfig{Mode: LLMLive, Model: "m", APIKey: "sk-secret"}}
	_, err := bad.Investigate(t.Context(), &Env{}, Scenario{ID: "x"})
	if err == nil || strings.Contains(err.Error(), "sk-secret") {
		t.Fatalf("a not-ready arm investigated or leaked its key: %v", err)
	}
}

// The arm's model client: the fake model's in fake mode, the configured
// endpoint and model in live mode. The key is never in the arm's text.
func TestLLMArm_ClientPerMode(t *testing.T) {
	fake, tap, done, err := LLMArm{Config: LLMConfig{Mode: LLMFake}}.tappedClient(
		Scenario{ID: "a"})
	if err != nil || fake == nil || !fake.IsEnabled() || fake.Model() != FakeModelName ||
		tap == nil {
		t.Fatalf("fake client %v tap %v (%v)", fake, tap, err)
	}
	done()
	cfg := LLMConfig{Mode: LLMLive, URL: "https://x.example/v1", Model: "gemini-2.5-flash",
		APIKey: "sk-secret"}
	live, tap, done, err := LLMArm{Config: cfg}.tappedClient(Scenario{ID: "a"})
	if err != nil || live == nil || !live.IsEnabled() || live.Model() != "gemini-2.5-flash" ||
		tap == nil || tap.upstream != "https://x.example/v1" {
		t.Fatalf("live client %v tap %v (%v)", live, tap, err)
	}
	done()
	for _, s := range []string{fmt.Sprintf("%v", LLMArm{Config: cfg}),
		fmt.Sprintf("%+v", LLMArm{Config: cfg}), fmt.Sprint(cfg)} {
		if strings.Contains(s, "sk-secret") {
			t.Fatalf("the key leaks: %s", s)
		}
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
