package tuning

import (
	"context"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/advisor"
	"github.com/pg-sage/sidecar/internal/analyzer"
)

// advisors-08: the tuning agent's instance settings (work_mem,
// autovacuum_*) go through the same instance-owner gate as the advisor's:
// on a database that does not own its instance they are not admitted.

func TestJudge_GUCNonOwnerNotAdmitted(t *testing.T) {
	s := defaultSettings()
	s.InstanceOwner = func() (string, bool) { return "alpha", false }
	h := newHarnessWith(t, s)
	for _, p := range []Proposal{
		gucProposal("work_mem", "64MB", -60),
		gucProposal("autovacuum_vacuum_scale_factor", "0.05", -30),
	} {
		j := judgeOne(t, h, nil, p)
		if j.Verdict != VerdictRejected || j.Reason != ReasonUnavailable {
			t.Fatalf("%s on a non-owner: %+v, want rejected unavailable", p.Name, j)
		}
		if !strings.Contains(j.Detail, "instance settings are managed from database alpha") {
			t.Fatalf("%s: detail %q does not name the owner", p.Name, j.Detail)
		}
	}
}

func TestJudge_GUCOwnerAndUnknownIdentityAdmitted(t *testing.T) {
	for _, tc := range []struct {
		name  string
		owner advisor.InstanceOwner
	}{
		{"owner", func() (string, bool) { return "alpha", true }},
		{"identity unknown", func() (string, bool) { return "", false }},
		{"no registry", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := defaultSettings()
			s.InstanceOwner = tc.owner
			j := judgeOne(t, newHarnessWith(t, s), nil, gucProposal("work_mem", "64MB", -60))
			if j.Verdict != VerdictAdmitted ||
				j.Finding.RecommendedSQL != "ALTER SYSTEM SET work_mem = '64MB'" {
				t.Fatalf("judged = %+v", j)
			}
			if j.Finding.Detail[advisor.DetailInstanceOwner] != nil {
				t.Fatalf("owner finding carries instance_owner: %v", j.Finding.Detail)
			}
		})
	}
}

// An open instance setting the agent stored before this database lost
// ownership stays open but loses its SQL: the non-owner's executor never
// runs an ALTER SYSTEM the owner did not propose.
func TestKeepOpen_NonOwnerDefersOpenGUC(t *testing.T) {
	open := analyzerGUC("ALTER SYSTEM SET work_mem = '64MB'")
	for _, tc := range []struct {
		name    string
		isOwner bool
		wantSQL string
	}{
		{"non-owner", false, ""},
		{"owner", true, open.RecommendedSQL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := defaultSettings()
			s.InstanceOwner = func() (string, bool) { return "alpha", tc.isOwner }
			h := newHarnessWith(t, s)
			_, cur := threeStatementPair()
			kept, _ := h.agent.keepOpen(context.Background(), cur, nil,
				[]analyzer.Finding{open}, nil)
			f, ok := byIdent(kept, open.ObjectIdentifier)
			if !ok || f.RecommendedSQL != tc.wantSQL {
				t.Fatalf("kept = %+v, want the open row with sql %q", kept, tc.wantSQL)
			}
			if !tc.isOwner && f.Detail[advisor.DetailInstanceOwner] != "alpha" {
				t.Fatalf("detail = %v, want instance_owner alpha", f.Detail)
			}
		})
	}
}

func analyzerGUC(sql string) analyzer.Finding {
	return analyzer.Finding{Category: "memory_tuning", Severity: "info",
		ObjectType: "configuration", ObjectIdentifier: "instance:work_mem",
		Title: "Set work_mem to 64MB", Detail: map[string]any{"setting": "work_mem"},
		Recommendation: "spills", RecommendedSQL: sql,
		RollbackSQL: "ALTER SYSTEM RESET work_mem", ActionRisk: "moderate"}
}
