package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/doctor"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/llm"
)

// No concurrent access tests: the handler keeps no state between requests;
// each request runs doctor once, sequentially.

type fakeDoctorProber struct{ calls int }

func (f *fakeDoctorProber) ProbeLLM(context.Context) llm.ProbeResult {
	f.calls++
	return llm.ProbeResult{Class: "ok", Model: "fake-model", LatencyMS: 3}
}

func doctorMgr(names ...string) *fleet.DatabaseManager {
	mgr := fleet.NewManager(&config.Config{Mode: "fleet"})
	for _, n := range names {
		mgr.RegisterInstance(&fleet.DatabaseInstance{Name: n,
			Config: config.DatabaseConfig{Name: n}})
	}
	return mgr
}

func serveDoctor(t *testing.T, d doctorDeps, query string) (*httptest.ResponseRecorder,
	doctor.Report) {
	t.Helper()
	w := httptest.NewRecorder()
	doctorHandler(d).ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/doctor"+query, nil))
	var r doctor.Report
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
			t.Fatalf("decode: %v\n%s", err, w.Body.String())
		}
	}
	return w, r
}

// Invalid input: several databases and no ?database=, an unknown name, a
// malformed name and an empty fleet are refused with their reason.
func TestDoctorRouteRefusals(t *testing.T) {
	cases := []struct {
		mgr   *fleet.DatabaseManager
		query string
		code  int
		text  string
	}{
		{doctorMgr("alpha", "beta"), "", http.StatusBadRequest, "?database="},
		{doctorMgr("alpha", "beta"), "?database=all", http.StatusBadRequest, "?database="},
		{doctorMgr("alpha"), "?database=gamma", http.StatusNotFound, "not found"},
		{doctorMgr("alpha"), "?database=bad%20name", http.StatusBadRequest, "invalid"},
		{doctorMgr(), "", http.StatusNotFound, "no monitored database"},
		{nil, "", http.StatusNotFound, "no monitored database"},
	}
	for _, tc := range cases {
		w, _ := serveDoctor(t, doctorDeps{mgr: tc.mgr}, tc.query)
		if w.Code != tc.code || !strings.Contains(w.Body.String(), tc.text) {
			t.Errorf("query %q: %d %s, want %d containing %q", tc.query, w.Code,
				w.Body.String(), tc.code, tc.text)
		}
	}
}

// Happy path without a pool: the one database is picked, its connection
// fails once, and the model is not probed unless asked for.
func TestDoctorRouteModelProbeIsOptIn(t *testing.T) {
	prober := &fakeDoctorProber{}
	d := doctorDeps{mgr: doctorMgr("alpha"), model: prober}
	w, r := serveDoctor(t, d, "")
	if w.Code != http.StatusOK || len(r.Checks) != 10 {
		t.Fatalf("status %d checks %d: %s", w.Code, len(r.Checks), w.Body.String())
	}
	if c, _ := r.Find(doctor.IDConnection); c.Status != doctor.StatusFail {
		t.Fatalf("connection without a pool = %+v, want fail", c)
	}
	model, _ := r.Find(doctor.IDModel)
	if model.Status != doctor.StatusUnknown || !strings.Contains(model.Detail,
		"probe_model=true") || prober.calls != 0 {
		t.Fatalf("model = %+v after %d probes, want unknown naming probe_model=true",
			model, prober.calls)
	}
	_, r = serveDoctor(t, d, "?database=alpha&probe_model=true")
	if model, _ = r.Find(doctor.IDModel); model.Status != doctor.StatusPass ||
		prober.calls != 1 {
		t.Fatalf("model = %+v after %d probes, want one passing probe", model, prober.calls)
	}
}

// Nil/empty: asking for the probe with no model configured says so.
func TestDoctorRouteNoModelConfigured(t *testing.T) {
	_, r := serveDoctor(t, doctorDeps{mgr: doctorMgr("alpha")}, "?probe_model=true")
	if c, _ := r.Find(doctor.IDModel); c.Status != doctor.StatusUnknown ||
		!strings.Contains(c.Detail, "no model configured") {
		t.Fatalf("model = %+v, want unknown \"no model configured\"", c)
	}
	if c, _ := r.Find(doctor.IDMetaDB); c.Status != doctor.StatusUnknown {
		t.Fatalf("meta without a meta DB = %+v, want unknown", c)
	}
}
