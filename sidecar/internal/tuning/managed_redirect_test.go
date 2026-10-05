package tuning

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/advisor"
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/managedparam"
)

// Managed clouds (roadmap phase 3): with host memory from provider
// telemetry, the agent's shared_buffers proposal on RDS/Aurora/Cloud SQL
// is no longer refused as "not executable here": it is redirected to the
// provider as a typed managed change an operator approves and applies.

func TestJudge_SharedBuffersOnManagedRedirectsWithTelemetry(t *testing.T) {
	for _, cloud := range []string{"rds", "aurora", "cloud-sql"} {
		s := defaultSettings()
		s.CloudEnv, s.DatabaseName = cloud, "app"
		s.HostMemory = func() advisor.HostMemory {
			return advisor.HostMemory{TotalBytes: 16 << 30, AvailableBytes: 8 << 30}
		}
		h := newHarnessWith(t, s)
		j := judgeOne(t, h, nil, gucProposal("shared_buffers", "4GB", -20))
		if j.Verdict != VerdictRedirected || j.Finding == nil {
			t.Fatalf("%s: judged = %+v", cloud, j)
		}
		f := j.Finding
		if f.RecommendedSQL != "" {
			t.Fatalf("%s: a managed change carries no SQL: %q", cloud, f.RecommendedSQL)
		}
		in, ok := managedparam.IntentFromDetail(f.Detail)
		if !ok || in.Parameter != "shared_buffers" || in.Value != "4GB" || in.Provider != cloud {
			t.Fatalf("%s: intent = %+v %t", cloud, in, ok)
		}
		if _, ok := f.Detail[analyzer.DetailApprovalRequired]; !ok {
			t.Fatalf("%s: approval required missing: %v", cloud, f.Detail)
		}
		if f.Detail["producer"] != Producer {
			t.Fatalf("%s: provenance must be recorded: %v", cloud, f.Detail)
		}
	}
}

// The live source wins over the static figure, and an unknown live value
// falls back to it.
func TestSettingsHostMemoryPrefersTelemetry(t *testing.T) {
	s := defaultSettings()
	s.HostMemoryBytes = 8 << 30
	if got := s.hostMemory(); got.TotalBytes != 8<<30 {
		t.Fatalf("static = %+v", got)
	}
	s.HostMemory = func() advisor.HostMemory { return advisor.HostMemory{TotalBytes: 32 << 30} }
	if got := s.hostMemory(); got.TotalBytes != 32<<30 {
		t.Fatalf("telemetry = %+v", got)
	}
	s.HostMemory = func() advisor.HostMemory { return advisor.HostMemory{} }
	if got := s.hostMemory(); got.TotalBytes != 8<<30 {
		t.Fatalf("unknown telemetry falls back: %+v", got)
	}
}

// Memory pressure from telemetry refuses a work_mem increase on RDS.
func TestJudge_WorkMemUnderMemoryPressureRefused(t *testing.T) {
	s := defaultSettings()
	s.CloudEnv, s.DatabaseName = "rds", "app"
	s.HostMemory = func() advisor.HostMemory {
		return advisor.HostMemory{TotalBytes: 16 << 30, AvailableBytes: 256 << 20}
	}
	h := newHarnessWith(t, s)
	j := judgeOne(t, h, nil, gucProposal("work_mem", "64MB", -60))
	if j.Verdict != VerdictRejected || j.Reason != ReasonUnavailable {
		t.Fatalf("judged = %+v", j)
	}
}
