package api

import (
	"fmt"
	"strings"
	"testing"
)

// Staging and retirement over the API (playbooks-10): an operator's edit
// of a signed runbook stages a draft while the signed version keeps
// running; a stale base_version is refused; only an admin retires.

func TestRunbookAPI_RetireNeedsAnAdmin(t *testing.T) {
	mgr, _, _ := sreFixture(t)
	id := createRB(t, mgr)
	path := rbBase + "/" + id + "/retire"
	code, out := rbCall(t, sreRouter(t, mgr, testOperatorUser()), "POST", path, "")
	if code != 403 {
		t.Fatalf("operator retire = %d %v, want 403", code, out)
	}
	viewer := sreRouter(t, mgr, testViewerUser())
	if code, out = rbCall(t, viewer, "GET", rbBase+"/"+id, ""); code != 200 ||
		out["status"] != "draft" {
		t.Fatalf("after the refused retire = %d %v, want the runbook untouched", code, out)
	}
	code, out = rbCall(t, sreRouter(t, mgr, testAdminUser()), "POST", path, "")
	if code != 200 || out["status"] != "retired" || out["retired_by"] != "user:1" {
		t.Fatalf("admin retire = %d %v, want 200 retired by user:1", code, out)
	}
}

func TestRunbookAPI_EditStagesADraftBesideTheSignedVersion(t *testing.T) {
	mgr, _, _ := sreFixture(t)
	id := createRB(t, mgr)
	admin := sreRouter(t, mgr, testAdminUser())
	op := sreRouter(t, mgr, testOperatorUser())
	sign1 := fmt.Sprintf(`{"version":1,"content_hash":%q}`, rbHash(t, rbDef))
	if code, out := rbCall(t, admin, "POST", rbBase+"/"+id+"/sign", sign1); code != 200 {
		t.Fatalf("sign v1 = %d %v", code, out)
	}
	edited := strings.Replace(rbDef, `"name":"Idle holder"`, `"name":"Idle holder v2"`, 1)
	code, out := rbCall(t, op, "POST", rbBase+"/"+id+"/versions",
		`{"base_version":1,"definition":`+edited+`}`)
	if code != 200 || out["latest_version"] != float64(2) || out["status"] != "draft" ||
		out["runnable"] != true || out["running_version"] != float64(1) {
		t.Fatalf("revise = %d %v; want draft v2 with v1 still running", code, out)
	}
	if latest := out["latest"].(map[string]any); latest["signed_at"] != nil ||
		latest["signature_valid"] != false {
		t.Fatalf("draft v2 = %v; a draft never inherits the signature", latest)
	}
	running, _ := out["running"].(map[string]any)
	if running["version"] != float64(1) || running["content_hash"] != rbHash(t, rbDef) {
		t.Fatalf("running = %v, want v1 over its own hash", running)
	}
	code, out = rbCall(t, op, "POST", rbBase+"/"+id+"/versions",
		`{"base_version":1,"definition":`+edited+`}`)
	if code != 409 || out["code"] != "version_conflict" {
		t.Fatalf("stale base_version = %d %v, want 409 version_conflict", code, out)
	}
	sign2 := fmt.Sprintf(`{"version":2,"content_hash":%q}`, rbHash(t, edited))
	code, out = rbCall(t, admin, "POST", rbBase+"/"+id+"/sign", sign2)
	if code != 200 || out["status"] != "signed" || out["running_version"] != float64(2) {
		t.Fatalf("sign v2 = %d %v; want v2 running", code, out)
	}
}
