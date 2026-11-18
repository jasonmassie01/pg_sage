package api

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/evidence/workpack"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// evid-02b: a build's body may opt in to plan conditions and value
// samples for that build only; both default off, and a build is bounded
// by the safety configuration's catalog timeouts. No concurrency test:
// parsing is a pure function of one request.

func TestParsePackWindow_OptInsDefaultOff(t *testing.T) {
	now := time.Date(2026, 11, 15, 10, 30, 0, 0, time.UTC)
	for body, want := range map[string][2]bool{
		``:                                {false, false},
		`{}`:                              {false, false},
		`{"include_conditions": true}`:    {true, false},
		`{"include_value_samples": true}`: {false, true},
		`{"include_conditions": true, "include_value_samples": true}`: {true, true},
	} {
		r := httptest.NewRequest("POST", "/", strings.NewReader(body))
		w, b, err := parsePackWindow(r, now)
		if err != nil || b.IncludeConditions != want[0] || b.IncludeValueSamples != want[1] ||
			w.To.IsZero() {
			t.Errorf("body %q: %+v %v, want opt-ins %v", body, b, err, want)
		}
	}
	r := httptest.NewRequest("POST", "/", strings.NewReader(`{"include_conditions": "yes"}`))
	if _, _, err := parsePackWindow(r, now); err == nil ||
		!strings.Contains(err.Error(), "include_conditions") {
		t.Fatalf("a non-boolean opt-in: %v, want an error naming the opt-ins", err)
	}
}

func TestPackRequest_UsesTheSafetyTimeouts(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Safety.QueryTimeoutMs = 1234
	req := packRequest(&fleet.DatabaseInstance{Name: "evid02b_none"}, cfg,
		workpack.DefaultWindowEnding(time.Now()), time.Now())
	if req.Timeouts != catalogread.FromSafety(cfg.Safety) ||
		req.Timeouts.Statement != 1234*time.Millisecond {
		t.Fatalf("timeouts %+v", req.Timeouts)
	}
	if req.Resource != "" || req.IncludeConditions || req.IncludeValueSamples {
		t.Fatalf("a database without cloud telemetry: %+v", req)
	}
}
