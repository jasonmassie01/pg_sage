package perfgate

import (
	"strings"
	"testing"
	"time"
)

func ms(n float64) time.Duration { return time.Duration(n * float64(time.Millisecond)) }

// One slow call on a loaded host is noise: the gate charges the median of
// the measured calls (after a discarded warm-up call), so a single
// 1.9 s outlier among fast calls no longer fails a healthy endpoint, and
// a consistently slow one still does.
func TestNewEndpointTakesTheMedian(t *testing.T) {
	e := NewEndpoint("/api/v1/trust", []int{200, 200, 200, 200, 200},
		[]time.Duration{ms(40), ms(1900), ms(35), ms(50), ms(45)})
	if e.Duration != ms(45) || e.Status != 200 {
		t.Fatalf("endpoint = %+v, want the median 45ms and 200", e)
	}
	if e.Max() != ms(1900) || len(e.Samples) != 5 {
		t.Fatalf("samples = %v (max %s), want all five kept", e.Samples, e.Max())
	}
	even := NewEndpoint("/x", []int{200, 200, 200, 200},
		[]time.Duration{ms(10), ms(40), ms(20), ms(30)})
	if even.Duration != ms(25) {
		t.Fatalf("even median = %s, want 25ms (mean of the middle two)", even.Duration)
	}
}

func TestNewEndpointAnyFailedCallFails(t *testing.T) {
	e := NewEndpoint("/x", []int{200, 503, 200}, []time.Duration{ms(1), ms(1), ms(1)})
	if e.Status != 503 {
		t.Fatalf("status = %d, want the failed call's 503", e.Status)
	}
	got, err := Evaluate([]Phase{withEndpoints(e)}, DefaultBudgets())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Gate != GateEndpoint || !strings.Contains(got[0].Detail, "503") {
		t.Fatalf("offenders = %+v, want the failed endpoint", got)
	}
}

func TestNewEndpointWithoutSamplesIsAnOffender(t *testing.T) {
	e := NewEndpoint("/x", nil, nil)
	if e.Status != 0 {
		t.Fatalf("status = %d, want 0 (never answered)", e.Status)
	}
	got, err := Evaluate([]Phase{withEndpoints(e)}, DefaultBudgets())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("unmeasured endpoint passed: %+v", got)
	}
}

func TestEndpointGateChargesTheMedian(t *testing.T) {
	b := DefaultBudgets()
	spike := NewEndpoint("/spike", []int{200, 200, 200, 200, 200},
		[]time.Duration{ms(30), ms(2085), ms(40), ms(1200), ms(35)})
	slow := NewEndpoint("/slow", []int{200, 200, 200, 200, 200},
		[]time.Duration{ms(1100), ms(1200), ms(90), ms(1300), ms(1050)})
	boundary := NewEndpoint("/edge", []int{200, 200, 200},
		[]time.Duration{ms(b.EndpointMaxMs), ms(b.EndpointMaxMs), ms(1)})
	got, err := Evaluate([]Phase{withEndpoints(spike, slow, boundary)}, b)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Subject != "/slow" || got[0].Measured != 1100 {
		t.Fatalf("offenders = %+v, want only /slow at its 1100ms median", got)
	}
	if !strings.Contains(got[0].Detail, "max 1300") {
		t.Fatalf("detail = %q, want the worst call named", got[0].Detail)
	}
}

func TestReportShowsMedianMaxAndSamples(t *testing.T) {
	e := NewEndpoint("/api/v1/trust", []int{200, 200, 200},
		[]time.Duration{ms(40), ms(900), ms(50)})
	md := RenderMarkdown(SmallScale(), DefaultBudgets(), []Phase{withEndpoints(e)}, nil)
	for _, want := range []string{"| endpoint | status | median ms | max ms | calls |",
		"| /api/v1/trust | 200 | 50.0 | 900.0 | 3 |"} {
		if !strings.Contains(md, want) {
			t.Fatalf("report lacks %q:\n%s", want, md)
		}
	}
}

func TestEndpointSamplesFromEnv(t *testing.T) {
	env := func(v string) func(string) string {
		return func(k string) string {
			if k == EnvEndpointSamples {
				return v
			}
			return ""
		}
	}
	if n, err := EndpointSamplesFromEnv(env("")); err != nil || n != DefaultEndpointSamples {
		t.Fatalf("default = %d, %v; want %d", n, err, DefaultEndpointSamples)
	}
	if n, err := EndpointSamplesFromEnv(env("9")); err != nil || n != 9 {
		t.Fatalf("9 = %d, %v", n, err)
	}
	for _, bad := range []string{"0", "-1", "two", "101"} {
		if _, err := EndpointSamplesFromEnv(env(bad)); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func withEndpoints(es ...Endpoint) Phase {
	p := steadyPhase()
	p.Endpoints = es
	return p
}
