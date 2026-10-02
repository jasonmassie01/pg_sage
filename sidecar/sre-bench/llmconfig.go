package srebench

import (
	"fmt"
	"net/url"
	"strings"
)

// LLM-on arm model selection (opt-in live endpoint; fake model otherwise).
const (
	EnvLLMURL   = "PG_SAGE_BENCH_LLM_URL"
	EnvLLMModel = "PG_SAGE_BENCH_LLM_MODEL"
	EnvLLMKey   = "PG_SAGE_BENCH_LLM_KEY"

	// LLMFake is the deterministic fake model CI uses.
	LLMFake = "fake"
	// LLMLive is an OpenAI-compatible endpoint.
	LLMLive = "live"
)

// LLMConfig is the model the LLM-on arm runs against. The endpoint and
// key never leave the process: the report records the mode and model.
type LLMConfig struct {
	Mode   string `json:"mode"`
	URL    string `json:"-"`
	Model  string `json:"model,omitempty"`
	APIKey string `json:"-"`
	// RPM caps the live model's calls per minute (0: unpaced); pace is
	// the pacer every run of the arm shares.
	RPM  int    `json:"rpm,omitempty"`
	pace *pacer `json:"-"`
}

// LLMConfigFromEnv reads the LLM-on arm's model: the fake model when no
// endpoint is set, else a live OpenAI-compatible endpoint, which needs a
// model and takes an optional key (local models have none).
func LLMConfigFromEnv(getenv func(string) string) (LLMConfig, error) {
	raw := strings.TrimSpace(getenv(EnvLLMURL))
	model := strings.TrimSpace(getenv(EnvLLMModel))
	key := strings.TrimSpace(getenv(EnvLLMKey))
	rpm, err := parseRPM(getenv(EnvLLMRPM))
	if err != nil {
		return LLMConfig{}, err
	}
	if raw == "" {
		if model != "" || key != "" || rpm != 0 {
			return LLMConfig{}, fmt.Errorf("%s, %s and %s need %s (an OpenAI-compatible "+
				"endpoint)", EnvLLMModel, EnvLLMKey, EnvLLMRPM, EnvLLMURL)
		}
		return LLMConfig{Mode: LLMFake}, nil
	}
	if model == "" {
		return LLMConfig{}, fmt.Errorf("%s is set but %s is empty", EnvLLMURL, EnvLLMModel)
	}
	u, err := url.Parse(raw)
	switch {
	case err != nil, u.Host == "", u.Scheme != "http" && u.Scheme != "https":
		// The parse error repeats the URL, which may hold secrets.
		return LLMConfig{}, fmt.Errorf("%s must be an http(s) URL", EnvLLMURL)
	case u.User != nil:
		return LLMConfig{}, fmt.Errorf("%s must not carry credentials; put the key in %s",
			EnvLLMURL, EnvLLMKey)
	}
	c := LLMConfig{Mode: LLMLive, URL: raw, Model: model, APIKey: key, RPM: rpm}
	if rpm > 0 {
		c.pace = newPacer(rpm)
	}
	return c, nil
}

// String names the model without the key.
func (c LLMConfig) String() string {
	if c.Mode != LLMLive {
		return "fake model"
	}
	host := "an unparsable endpoint"
	if u, err := url.Parse(c.URL); err == nil {
		host = u.Host
	}
	return fmt.Sprintf("live model %q at %s", c.Model, host)
}
