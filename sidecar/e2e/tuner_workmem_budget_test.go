//go:build e2e

package e2e

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"testing"

	"github.com/pg-sage/sidecar/internal/advisor"
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/tuner"
)

// tuning-03 CHECK-02: no tuning-agent hint the tuner admits exceeds
// min(tuner.work_mem_max_mb, 5% of host memory), and with host memory
// unknown every work_mem hint waits for an operator. The deterministic
// rules' cap is pinned on real PostgreSQL in internal/tuner
// (TestStatSpillDB_PlanHintIsCappedByHostMemory).

var workMemMB = regexp.MustCompile(`Set\(work_mem "(\d+)MB"\)`)

func admittedWorkMem(t *testing.T, f analyzer.Finding) int {
	t.Helper()
	m := workMemMB.FindStringSubmatch(fmt.Sprint(f.Detail["hint_directive"]))
	if m == nil {
		t.Fatalf("admitted hint has no work_mem: %v", f.Detail["hint_directive"])
	}
	mb, _ := strconv.Atoi(m[1])
	return mb
}

func TestTunerWorkMemBudget_AgentHintsStayUnderTheCap(t *testing.T) {
	for _, host := range []struct {
		bytes   int64
		ceiling int
	}{{4 << 30, 204}, {16 << 30, 512}, {0, 512}} {
		tu := tuner.New(nil, tuner.TunerConfig{WorkMemMaxMB: 512},
			&tuner.HintPlanAvailability{ApplicationEffect: true, HintTableReady: true}, nil)
		total := host.bytes
		tu.WithHostMemory(func() advisor.HostMemory {
			return advisor.HostMemory{TotalBytes: total}
		})
		for i, mb := range []int{8, 64, 204, 205, 256, 512, 2048} {
			p := tuner.HintProposal{QueryID: int64(1000 + i), Query: "SELECT 1",
				Hint: fmt.Sprintf(`Set(work_mem "%dMB")`, mb)}
			f, err := tu.CheckHint(context.Background(), p)
			if errors.Is(err, tuner.ErrHintMemory) {
				if total == 0 || mb <= host.ceiling {
					t.Fatalf("host %d: %dMB refused: %v", total, mb, err)
				}
				continue
			}
			if err != nil {
				t.Fatalf("host %d: %dMB: %v", total, mb, err)
			}
			if got := admittedWorkMem(t, f); got > host.ceiling {
				t.Fatalf("CHECK-02: host %d admitted %dMB above the %dMB cap", total, got,
					host.ceiling)
			}
			_, gated := f.Detail[analyzer.DetailApprovalRequired]
			if gated != (total == 0) {
				t.Fatalf("host %d: approval_required = %v, want it only when host memory is "+
					"unknown", total, gated)
			}
		}
	}
}
