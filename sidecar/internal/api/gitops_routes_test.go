package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/gitops"
	"github.com/pg-sage/sidecar/internal/policy"
)

// cli-02a: the Settings banner's source. No concurrent test here: the
// report is a copy (gitops.Status is tested for concurrent use).
func TestGitopsRoute_ReportsTheLastReconcile(t *testing.T) {
	mux := http.NewServeMux()
	registerGitopsRoutes(mux)
	if rec := requestPolicy(mux, http.MethodGet, "/api/v1/gitops", "",
		viewerFixture.user()); rec.Code != http.StatusForbidden {
		t.Fatalf("a viewer read the reconcile report: %d", rec.Code)
	}
	get := func() map[string]any {
		t.Helper()
		rec := requestPolicy(mux, http.MethodGet, "/api/v1/gitops", "", adminFixture.user())
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	SetGitopsReport(nil)
	t.Cleanup(func() { SetGitopsReport(nil) })
	if got := get(); got["configured"] != false || got["report"] != nil {
		t.Fatalf("unconfigured = %v", got)
	}
	var s gitops.Status
	s.Record("/etc/sage/policy", gitops.Result{Mode: gitops.ModeDryRun, Hash: "h1"}, nil,
		time.Now())
	SetGitopsReport(s.Report)
	got := get()
	report, _ := got["report"].(map[string]any)
	if got["configured"] != true || report["hash"] != "h1" || report["mode"] != "dry_run" {
		t.Fatalf("configured report = %v", got)
	}
	if changes, ok := report["changes"].([]any); !ok || len(changes) != 0 {
		t.Fatalf("changes = %#v, want an empty list", report["changes"])
	}
	s.Record("/etc/sage/policy", gitops.Result{}, &gitops.InvalidError{
		Problems: []gitops.Problem{{File: "grants/g.yaml", Line: 4, Message: "bad"}}},
		time.Now())
	report, _ = get()["report"].(map[string]any)
	problems, _ := report["problems"].([]any)
	if report["error"] == "" || len(problems) != 1 {
		t.Fatalf("failed report = %v", report)
	}
}

func TestPolicySetView_CarriesTheOwner(t *testing.T) {
	row := policySetView(policy.SetDocument{Set: policy.PolicySet{Name: "prod"},
		ManagedBy: policy.ManagedByConfig})
	raw, err := json.Marshal(row)
	if err != nil || !strings.Contains(string(raw), `"managed_by":"config"`) {
		t.Fatalf("view = %s, %v", raw, err)
	}
}
