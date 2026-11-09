package tuning

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/advisor"
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/tuner"
)

// tuning-03: an agent work_mem hint passes the host-memory rule the agent's
// GUC proposals pass (5% of host RAM); with host memory unknown it needs an
// operator's approval. The real tuner judges it here, not the fake sink.

func realHintSink(h *harness, maxMB int, hostBytes int64) *tuner.Tuner {
	tu := tuner.New(nil, tuner.TunerConfig{WorkMemMaxMB: maxMB},
		&tuner.HintPlanAvailability{ApplicationEffect: true, HintTableReady: true}, noLog)
	tu.WithHostMemory(func() advisor.HostMemory {
		return advisor.HostMemory{TotalBytes: hostBytes}
	})
	h.agent.deps.Hints = tu
	return tu
}

func workMemHint(value string) Proposal {
	p := hintProposal(101)
	p.Hint = `Set(work_mem "` + value + `")`
	return p
}

func TestJudge_QueryHintWorkMemAboveHostCapRefused(t *testing.T) {
	h := newHarness(t)
	realHintSink(h, 512, 4<<30)
	j := judgeOne(t, h, nil, workMemHint("512MB"))
	if j.Verdict != VerdictRejected || j.Reason != ReasonInvalid ||
		!strings.Contains(j.Detail, "above 5% of host memory") {
		t.Fatalf("judged = %+v", j)
	}
	j = judgeOne(t, h, nil, workMemHint("204MB"))
	if j.Verdict != VerdictAdmitted || j.Finding.Detail["hint_directive"] !=
		`Set(work_mem "204MB")` {
		t.Fatalf("at the cap: %+v", j)
	}
	if _, gated := j.Finding.Detail[analyzer.DetailApprovalRequired]; gated {
		t.Fatalf("a known host needs no approval: %v", j.Finding.Detail)
	}
}

func TestJudge_QueryHintWorkMemUnknownHostNeedsApproval(t *testing.T) {
	h := newHarness(t)
	realHintSink(h, 512, 0)
	j := judgeOne(t, h, nil, workMemHint("512MB"))
	if j.Verdict != VerdictAdmitted {
		t.Fatalf("judged = %+v", j)
	}
	why, _ := j.Finding.Detail[analyzer.DetailApprovalRequired].(string)
	if !strings.Contains(why, "host memory is unknown") ||
		j.Finding.Detail["producer"] != Producer {
		t.Fatalf("detail = %v", j.Finding.Detail)
	}
}

func TestSystemPrompt_WorkMemRuleCitesTheComputedCap(t *testing.T) {
	s := defaultSettings()
	s.WorkMemMaxMB = 512
	s.HostMemoryBytes = 4 << 30
	got := New(s, Deps{}, nil).systemPrompt()
	if !strings.Contains(got, "204MB") || strings.Contains(got, "256MB") ||
		!strings.Contains(got, "5% of host memory") {
		t.Fatalf("rule 8 on a 4 GiB host:\n%s", ruleLine(got, "8."))
	}
	s.HostMemoryBytes = 0
	got = New(s, Deps{}, nil).systemPrompt()
	if !strings.Contains(ruleLine(got, "8."), "512MB") ||
		!strings.Contains(ruleLine(got, "8."), "host memory is unknown") {
		t.Fatalf("rule 8 with host memory unknown:\n%s", ruleLine(got, "8."))
	}
	s.WorkMemMaxMB = 0
	if line := ruleLine(New(s, Deps{}, nil).systemPrompt(), "8."); !strings.Contains(line,
		"512MB") {
		t.Fatalf("an unset tuner.work_mem_max_mb uses the default 512MB: %s", line)
	}
	if !strings.Contains(got, "1. Propose only the allowed types") ||
		!strings.Contains(got, "11. If nothing is warranted") {
		t.Fatalf("the other rules are unchanged:\n%s", got)
	}
}

func ruleLine(prompt, prefix string) string {
	for _, l := range strings.Split(prompt, "\n") {
		if strings.HasPrefix(l, prefix) {
			return l
		}
	}
	return ""
}
