package srebench

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Pacing a live model (M4): the first live run hit the provider's rate
// limit once the replay corpus sent its calls back to back (63 of 64
// replay calls failed). PG_SAGE_BENCH_LLM_RPM spaces every call the
// LLM-on arm makes, across all its runs, so a rate-limited key can still
// measure the model. Unset means unpaced.

func TestLLMConfigFromEnv_RPM(t *testing.T) {
	live := liveVars(map[string]string{EnvLLMModel: "m"})
	with := func(rpm string) map[string]string {
		m := map[string]string{EnvLLMRPM: rpm}
		for k, v := range live {
			m[k] = v
		}
		return m
	}
	got, err := LLMConfigFromEnv(env(with(" 10 ")))
	if err != nil || got.RPM != 10 || got.pace == nil || got.pace.interval != 6*time.Second {
		t.Fatalf("rpm 10 = %+v (%v)", got, err)
	}
	if got, err := LLMConfigFromEnv(env(live)); err != nil || got.RPM != 0 || got.pace != nil {
		t.Fatalf("no rpm = %+v (%v), want unpaced", got, err)
	}
	for _, bad := range []string{"0", "-1", "x", "6001", "1.5"} {
		if _, err := LLMConfigFromEnv(env(with(bad))); err == nil ||
			!strings.Contains(err.Error(), EnvLLMRPM) {
			t.Errorf("rpm %q: err %v, want one naming %s", bad, err, EnvLLMRPM)
		}
	}
	if _, err := LLMConfigFromEnv(env(map[string]string{EnvLLMRPM: "10"})); err == nil {
		t.Error("an rpm without a live endpoint was accepted")
	}
}

func TestPacer_SpacesCallsAcrossCallers(t *testing.T) {
	p := newPacer(1200) // 50 ms apart
	ctx := context.Background()
	start := time.Now()
	for i := 0; i < 4; i++ {
		if err := p.wait(ctx); err != nil {
			t.Fatalf("wait: %v", err)
		}
	}
	if d := time.Since(start); d < 150*time.Millisecond {
		t.Fatalf("4 calls at 1200 rpm took %s, want at least 150 ms", d)
	}
}

func TestPacer_CanceledWaitReturnsTheError(t *testing.T) {
	p := newPacer(1) // one call a minute
	if err := p.wait(context.Background()); err != nil {
		t.Fatalf("first wait: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := p.wait(ctx); err == nil {
		t.Fatal("a wait past its context returned nil")
	}
	var nilPacer *pacer
	if err := nilPacer.wait(context.Background()); err != nil {
		t.Fatalf("a nil pacer must not wait: %v", err)
	}
}

func TestModelTap_PacedForwarding(t *testing.T) {
	var auth atomic.Value
	var hits atomic.Int32
	up := upstream(t, http.StatusOK, `{"choices":[]}`, &auth)
	tap := NewModelTap(up.URL)
	tap.pace = newPacer(1200)
	t.Cleanup(tap.Close)
	start := time.Now()
	for i := 0; i < 3; i++ {
		if resp, _ := chat(t, tap.URL(), "x"); resp.StatusCode == http.StatusOK {
			hits.Add(1)
		}
	}
	if hits.Load() != 3 || time.Since(start) < 100*time.Millisecond {
		t.Fatalf("3 paced calls: %d ok in %s", hits.Load(), time.Since(start))
	}
}
