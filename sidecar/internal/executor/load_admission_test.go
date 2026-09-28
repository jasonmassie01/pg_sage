package executor

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/verify"
)

const testMiB = 1024 * 1024

type fakeIOEvidence struct {
	evidence verify.IOEvidence
	err      error
}

func (f fakeIOEvidence) IOEvidence(context.Context) (verify.IOEvidence, error) {
	return f.evidence, f.err
}

func ioEvidenceAt(dataMBps, walMBps float64, baseline verify.IOBaseline) verify.IOEvidence {
	return verify.IOEvidence{
		Rate: &verify.IORate{
			At: time.Now(), Interval: time.Minute, Source: verify.IOSourcePGStatIO,
			DataBytesPerSec: dataMBps * testMiB, WALBytesPerSec: walMBps * testMiB,
		},
		Baseline: baseline,
	}
}

func quietIOEvidence() verify.IOEvidence {
	return ioEvidenceAt(1, 1, verify.IOBaseline{})
}

func declaredConfig(readWrite, wal float64) *config.Config {
	cfg := config.DefaultConfig()
	cfg.Verify.IOCapacity = &config.IOCapacityConfig{ReadWriteMBps: readWrite, WALMBps: wal}
	return cfg
}

func admissionEngine(t *testing.T, e *Executor) *verify.Engine {
	t.Helper()
	engine, err := verify.NewEngine(evidenceSource(e),
		verify.NewPostgresStateStore(nil, 1), verificationOptions(e.cfg))
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	return engine
}

type windowGate struct {
	custodianGateCapture
	open bool
	err  error
}

func (g *windowGate) MaintenanceWindowOpen(
	context.Context, policy.ActionRequest,
) (bool, error) {
	return g.open, g.err
}

func TestSupabaseCPUPlusDeclaredIOAdmits(t *testing.T) {
	e := &Executor{cfg: declaredConfig(1000, 100)}
	e.WithHostCPUReader(testHostCPU{cpu: 25})
	e.WithIOEvidence(fakeIOEvidence{evidence: ioEvidenceAt(100, 10, verify.IOBaseline{})})
	admission, err := admissionEngine(t, e).OKToApplyNow(context.Background())
	if err != nil || !admission.OK || admission.Mode != verify.EvidenceDeclaredCapacity {
		t.Fatalf("admission = %+v, %v; want declared-capacity admit", admission, err)
	}
	if admission.Evidence["data_io_pct"] != 10.0 || admission.Evidence["cpu_pct"] != 25.0 {
		t.Fatalf("evidence = %#v", admission.Evidence)
	}
}

func TestIOCeilingIndependentOfCPUCeiling(t *testing.T) {
	cfg := declaredConfig(1000, 100)
	cfg.Safety.CPUCeilingPct, cfg.Safety.DataIOCeilingPct = 90, 60
	options := verificationOptions(cfg)
	if options.CPUCeilingPct != 90 || options.DataIOCeilingPct != 60 {
		t.Fatalf("ceilings = cpu %v data %v", options.CPUCeilingPct, options.DataIOCeilingPct)
	}
	e := &Executor{cfg: cfg}
	e.WithHostCPUReader(testHostCPU{cpu: 10})
	e.WithIOEvidence(fakeIOEvidence{evidence: ioEvidenceAt(650, 1, verify.IOBaseline{})})
	admission, err := admissionEngine(t, e).OKToApplyNow(context.Background())
	if err != nil || admission.OK || admission.Reason != verify.ReasonDataIOCeiling {
		t.Fatalf("65%% data IO under a 60%% ceiling = %+v, %v", admission, err)
	}
}

func TestVerificationOptionsDefaultIOAdmission(t *testing.T) {
	options := verificationOptions(config.DefaultConfig())
	if options.BaselineDays != 7 {
		t.Fatalf("default baseline days = %v, want 7", options.BaselineDays)
	}
	if options.DataIOCeilingPct != 70 || options.LogIOCeilingPct != 70 {
		t.Fatalf("default IO ceilings = %v/%v, want 70/70",
			options.DataIOCeilingPct, options.LogIOCeilingPct)
	}
	if options.CPUCeilingPct != float64(config.DefaultCPUCeilingPct) {
		t.Fatalf("CPU ceiling = %v", options.CPUCeilingPct)
	}
	cfg := config.DefaultConfig()
	cfg.Verify.IOBaselineDays = 0
	if got := verificationOptions(cfg).BaselineDays; got != 0 {
		t.Fatalf("explicit 0 must disable the baseline, got %v", got)
	}
}

func TestLoadEvidenceWithoutIOSamplerIsUnavailable(t *testing.T) {
	e := &Executor{cfg: config.DefaultConfig()}
	e.WithHostCPUReader(testHostCPU{cpu: 1})
	admission, err := admissionEngine(t, e).OKToApplyNow(context.Background())
	if err != nil || admission.OK || admission.Reason != verify.ReasonLoadUnavailable ||
		admission.Mode != verify.EvidenceUnavailable {
		t.Fatalf("admission without sampler = %+v, %v", admission, err)
	}
}

func TestLoadEvidenceSamplerErrorFailsClosed(t *testing.T) {
	e := &Executor{cfg: declaredConfig(1000, 100)}
	e.WithIOEvidence(fakeIOEvidence{err: errors.New("sage.io_rate_sample unreadable")})
	admission, err := admissionEngine(t, e).OKToApplyNow(context.Background())
	if err == nil || admission.OK || admission.Reason != verify.ReasonLoadUnavailable {
		t.Fatalf("sampler failure = %+v, %v", admission, err)
	}
}

func TestLoadEvidenceUsesStandingPolicyWindow(t *testing.T) {
	e := &Executor{cfg: config.DefaultConfig()}
	e.WithIOEvidence(fakeIOEvidence{evidence: quietIOEvidence()})
	source := evidenceSource(e)
	cases := []struct {
		name string
		gate policy.Gate
		want bool
	}{
		{"no gate", nil, false},
		{"gate without window report", &custodianGateCapture{}, false},
		{"window open", &windowGate{open: true}, true},
		{"window closed", &windowGate{open: false}, false},
		{"window unknown", &windowGate{open: true, err: errors.New("policy down")}, false},
	}
	for _, test := range cases {
		e.WithPolicyGate(test.gate)
		got, err := source.LoadEvidence(context.Background())
		if err != nil || got.WindowOpen != test.want {
			t.Fatalf("%s: window=%v err=%v, want %v", test.name, got.WindowOpen, err, test.want)
		}
	}
}

func TestLoadEvidenceCarriesDeclaredCapacityPerDatabase(t *testing.T) {
	e := &Executor{cfg: declaredConfig(750, 125)}
	e.WithIOEvidence(fakeIOEvidence{evidence: quietIOEvidence()})
	got, err := evidenceSource(e).LoadEvidence(context.Background())
	if err != nil || got.Capacity == nil || got.Capacity.ReadWriteMBps != 750 ||
		got.Capacity.WALMBps != 125 {
		t.Fatalf("capacity = %+v, %v", got.Capacity, err)
	}
	e.cfg.Verify.IOCapacity = nil
	got, err = evidenceSource(e).LoadEvidence(context.Background())
	if err != nil || got.Capacity != nil {
		t.Fatalf("absent attestation produced capacity %+v, %v", got.Capacity, err)
	}
}

func TestAdmissionWithheldErrorIdentifiesReason(t *testing.T) {
	err := error(&AdmissionWithheldError{Admission: verify.Admission{
		Reason: verify.ReasonLearningBaseline, Mode: verify.EvidenceLearnedBaseline,
		Detail: "learning IO baseline: 3.0/7 days",
	}})
	if !errors.Is(err, ErrVerificationUnavailable) {
		t.Fatalf("withheld admission must wrap ErrVerificationUnavailable: %v", err)
	}
	for _, want := range []string{"learning_baseline", "3.0/7 days"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing %q", err, want)
		}
	}
	var withheld *AdmissionWithheldError
	if !errors.As(fmtWrap(err), &withheld) || withheld.Admission.Mode == "" {
		t.Fatal("withheld admission lost through wrapping")
	}
}

func fmtWrap(err error) error { return errors.Join(errors.New("context"), err) }

func TestIndexAdmissionStatusWithoutVerification(t *testing.T) {
	e := New(nil, config.DefaultConfig(), nil, zeroTime(), func(string, string, ...any) {})
	e.WithDatabaseName("orders_db")
	status := e.IndexAdmissionStatus(context.Background())
	if status.OK || status.Reason != verify.ReasonLoadUnavailable ||
		status.Mode != verify.EvidenceUnavailable || status.Database != "orders_db" {
		t.Fatalf("status = %+v", status)
	}
	for _, missing := range []string{"pg_io_rate", "host_cpu"} {
		if !slices.Contains(status.MissingEvidence, missing) {
			t.Fatalf("missing evidence %v lacks %q", status.MissingEvidence, missing)
		}
	}
	if status.BaselineRequiredDays != 7 || status.CheckedAt.IsZero() {
		t.Fatalf("status baseline/time = %+v", status)
	}
}
