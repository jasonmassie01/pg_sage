package api

import (
	"net/http"
	"sync"

	"github.com/pg-sage/sidecar/internal/gitops"
)

// GET /api/v1/gitops (cli-02a) is the GitOps directory's last reconcile
// for the Settings banner: its mode, hash and diff, or its error with
// every problem's file and line. Admin only: the diff names principals
// and users. {configured: false} when policy.directory is off.

var gitopsReport struct {
	mu sync.Mutex
	fn func() gitops.Report
}

// SetGitopsReport installs the reconciler's status (nil: not configured).
func SetGitopsReport(fn func() gitops.Report) {
	gitopsReport.mu.Lock()
	defer gitopsReport.mu.Unlock()
	gitopsReport.fn = fn
}

func currentGitopsReport() (gitops.Report, bool) {
	gitopsReport.mu.Lock()
	fn := gitopsReport.fn
	gitopsReport.mu.Unlock()
	if fn == nil {
		return gitops.Report{}, false
	}
	return fn(), true
}

func registerGitopsRoutes(mux *http.ServeMux) {
	route(mux, "GET /api/v1/gitops", gateAdmin, http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			report, ok := currentGitopsReport()
			body := map[string]any{"configured": ok}
			if prs := approvalPRStatus(); prs != nil {
				body["approval_prs"] = prs // cli-02b
			}
			if !ok {
				jsonResponse(w, body)
				return
			}
			if report.Changes == nil {
				report.Changes = []gitops.Change{}
			}
			body["report"] = report
			jsonResponse(w, body)
		}))
}
