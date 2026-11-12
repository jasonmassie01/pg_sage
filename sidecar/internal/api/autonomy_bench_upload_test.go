package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/benchsig"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/earned"
)

// benches-09: an admin uploads a signed release report as multipart/form-
// data with a "report" and a "bundle" part (1 MiB together). The bundle is
// verified against the release identity and the report is stored as a
// signed release report that must name the running build; a body-only
// upload stays an unsigned operator report.

// uploadVerifier accepts a bundle reading "valid:<report>", signed for
// commit.
type uploadVerifier struct{ commit string }

func (v uploadVerifier) Verify(report, bundle []byte) (benchsig.Signature, error) {
	if !bytes.Equal(bundle, append([]byte("valid:"), report...)) {
		return benchsig.Signature{}, benchsig.ErrInvalidSignature
	}
	return benchsig.Signature{Identity: "https://github.com/jasonmassie01/pg_sage/" +
		".github/workflows/ci.yml@refs/tags/v1.8.5", Issuer: benchsig.GitHubIssuer,
		Commit: v.commit, SignedAt: time.Now().UTC()}, nil
}

// signedUploadFixture rebinds orders to a ledger running uploadBuild and
// routes as user with verifier (none when nil).
func signedUploadFixture(t *testing.T, user *auth.User,
	verifier BenchVerifier) (http.Handler, *earned.Service) {
	t.Helper()
	f := newAutonomyAPIFixture(t)
	f.useReleaseBuild()
	inject := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r = r.WithContext(context.WithValue(r.Context(), userContextKey, user))
			next.ServeHTTP(w, r)
		})
	}
	deps := &AutonomyDeps{Ledgers: f.reg}
	if verifier != nil {
		deps.BenchVerifier = verifier
	}
	return newTestRouter(f.mgr, config.DefaultConfig(), nil, nil, nil, nil,
		&RuntimeDeps{Autonomy: deps}, inject), f.ledger
}

// stampedBench is benchBody naming uploadBuild.
func stampedBench(at time.Time) string {
	return strings.Replace(benchBody(at), `"generated_at"`, fmt.Sprintf(
		`"pg_sage_version":%q,"pg_sage_commit":%q,"generated_at"`, uploadBuild.Version,
		uploadBuild.Commit), 1)
}

// uploadParts posts parts (name -> content) as multipart/form-data.
func uploadParts(t *testing.T, h http.Handler, parts map[string]string) (int,
	map[string]any) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for _, name := range []string{"report", "bundle", "other"} {
		content, ok := parts[name]
		if !ok {
			continue
		}
		part, err := mw.CreateFormFile(name, name+".json")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/api/v1/sre/autonomy/bench-results?database=orders",
		&body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	out := map[string]any{}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("non-JSON body %q", w.Body.String())
	}
	return w.Code, out
}

func TestBenchUploadAPI_SignedMultipartIsASignedReleaseReport(t *testing.T) {
	h, ledger := signedUploadFixture(t, testAdminUser(), uploadVerifier{commit: uploadCommit})
	report := stampedBench(time.Now().UTC().Add(-time.Hour))
	code, body := uploadParts(t, h, map[string]string{"report": report,
		"bundle": "valid:" + report})
	run, _ := body["run"].(map[string]any)
	sig, _ := run["signature"].(map[string]any)
	if code != 201 || run["origin"] != earned.OriginSignedRelease || sig == nil ||
		sig["commit"] != uploadCommit ||
		!strings.Contains(fmt.Sprint(run["provenance"]), "signed release") {
		t.Fatalf("signed upload = %d %v", code, body)
	}
	ev, err := ledger.Evidence(context.Background(), earned.FamilyLockBlocking,
		earned.ClassBackendCancel)
	if err != nil || ev.Bench == nil || ev.Bench.Origin != earned.OriginSignedRelease ||
		ev.Bench.IngestedBy != "user:1:admin@test.com" {
		t.Fatalf("stored evidence = %+v (%v)", ev.Bench, err)
	}
	// The same report body-only is the duplicate, still signed.
	code, body = autonomyCall(t, h, "POST",
		"/api/v1/sre/autonomy/bench-results?database=orders", report)
	if run, _ = body["run"].(map[string]any); code != 200 || run["duplicate"] != true ||
		run["origin"] != earned.OriginSignedRelease {
		t.Fatalf("body-only duplicate = %d %v", code, body)
	}
}

func TestBenchUploadAPI_BodyOnlyUploadStaysOperator(t *testing.T) {
	h, _ := signedUploadFixture(t, testAdminUser(), uploadVerifier{commit: uploadCommit})
	code, body := autonomyCall(t, h, "POST",
		"/api/v1/sre/autonomy/bench-results?database=orders",
		stampedBench(time.Now().UTC().Add(-time.Hour)))
	run, _ := body["run"].(map[string]any)
	if code != 201 || run["origin"] != earned.OriginOperator || run["signature"] != nil {
		t.Fatalf("body-only upload = %d %v, want an unsigned operator report", code, body)
	}
}

func TestBenchUploadAPI_RefusesBadSignedUploads(t *testing.T) {
	report := stampedBench(time.Now().UTC().Add(-time.Hour))
	otherCommit := strings.Repeat("d", 40)
	other := strings.Replace(report, uploadCommit, otherCommit, 1)
	big := strings.Repeat(" ", 1<<20+1) + report
	cases := []struct {
		name   string
		parts  map[string]string
		commit string
		want   string
	}{
		{"bad bundle", map[string]string{"report": report, "bundle": "valid:x"},
			uploadCommit, "signature"},
		{"no bundle", map[string]string{"report": report}, uploadCommit, "bundle"},
		{"no report", map[string]string{"bundle": "valid:" + report}, uploadCommit, "report"},
		{"empty bundle", map[string]string{"report": report, "bundle": ""}, uploadCommit,
			"bundle"},
		{"unknown part", map[string]string{"report": report, "bundle": "valid:" + report,
			"other": "x"}, uploadCommit, "other"},
		{"signed for another commit", map[string]string{"report": report,
			"bundle": "valid:" + report}, otherCommit, "commit"},
		{"another build", map[string]string{"report": other, "bundle": "valid:" + other},
			otherCommit, "another pg_sage build"},
		{"report over 1 MiB", map[string]string{"report": big, "bundle": "valid:" + big},
			uploadCommit, "1 MiB"},
	}
	for _, c := range cases {
		h, ledger := signedUploadFixture(t, testAdminUser(), uploadVerifier{commit: c.commit})
		code, body := uploadParts(t, h, c.parts)
		if code != 400 || !strings.Contains(fmt.Sprint(body["error"]), c.want) {
			t.Errorf("%s: upload = %d %v, want 400 naming %q", c.name, code, body, c.want)
		}
		if v, err := ledger.View(context.Background()); err != nil || v.Bench != nil {
			t.Errorf("%s: a refused upload was stored: %+v (%v)", c.name, v.Bench, err)
		}
	}
}

func TestBenchUploadAPI_NonAdminIsForbidden(t *testing.T) {
	report := stampedBench(time.Now().UTC().Add(-time.Hour))
	for _, user := range []*auth.User{testOperatorUser(), testViewerUser()} {
		h, ledger := signedUploadFixture(t, user, uploadVerifier{commit: uploadCommit})
		code, body := uploadParts(t, h, map[string]string{"report": report,
			"bundle": "valid:" + report})
		if code != 403 {
			t.Fatalf("%s signed upload = %d %v, want 403", user.Email, code, body)
		}
		if v, err := ledger.View(context.Background()); err != nil || v.Bench != nil {
			t.Fatalf("a forbidden upload was stored: %+v (%v)", v.Bench, err)
		}
	}
}

// Without verification material a signed upload fails closed.
func TestBenchUploadAPI_WithoutAVerifierIsUnavailable(t *testing.T) {
	h, ledger := signedUploadFixture(t, testAdminUser(), nil)
	report := stampedBench(time.Now().UTC().Add(-time.Hour))
	code, body := uploadParts(t, h, map[string]string{"report": report,
		"bundle": "valid:" + report})
	if code != 503 || body["code"] != "signature_unavailable" {
		t.Fatalf("signed upload without a verifier = %d %v, want 503", code, body)
	}
	if v, err := ledger.View(context.Background()); err != nil || v.Bench != nil {
		t.Fatalf("an unverified upload was stored: %+v (%v)", v.Bench, err)
	}
}

// No concurrent-access test: the handler holds no state of its own; the
// ledger's concurrent ingest is covered by
// earned.TestModelLift_ConcurrentDuplicateIngestStoresOnce.

// multipartBench is a signed-upload-shaped multipart POST to path.
func multipartBench(t *testing.T, path, fetchSite string) *http.Request {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for _, name := range []string{"report", "bundle"} {
		part, err := mw.CreateFormFile(name, name+".json")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = part.Write([]byte(`{}`))
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "http://sage.example"+path, &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.AddCookie(&http.Cookie{Name: "sage_session", Value: "admin-session"})
	if fetchSite != "" {
		req.Header.Set("Sec-Fetch-Site", fetchSite)
		req.Header.Set("Origin", "https://sibling.sage.example")
	}
	return req
}

// The multipart exemption from the JSON content-type check is exact: the
// cross-origin check still refuses a same-site form posting a signed upload
// (apiauth-10), and other write routes still refuse multipart.
func TestBenchUploadAPI_MultipartExemptionKeepsTheCrossOriginCheck(t *testing.T) {
	const benchPath = "/api/v1/sre/autonomy/bench-results"
	for _, site := range []string{"same-site", "cross-site"} {
		h, reached := csrfRouter(t, config.DefaultConfig(), nil)
		rec := serveCSRF(h, multipartBench(t, benchPath, site))
		if rec.Code != http.StatusForbidden || *reached {
			t.Fatalf("%s signed upload = %d reached=%v, want 403 unreached", site, rec.Code,
				*reached)
		}
	}
	h, reached := csrfRouter(t, config.DefaultConfig(), nil)
	if rec := serveCSRF(h, multipartBench(t, benchPath, "same-origin")); rec.Code !=
		http.StatusAccepted || !*reached {
		t.Fatalf("same-origin signed upload = %d reached=%v, want 202 reached", rec.Code,
			*reached)
	}
	for _, path := range []string{"/api/v1/emergency-stop", benchPath + "/x"} {
		h, reached = csrfRouter(t, config.DefaultConfig(), nil)
		if rec := serveCSRF(h, multipartBench(t, path, "")); rec.Code !=
			http.StatusUnsupportedMediaType || *reached {
			t.Fatalf("multipart POST to %s = %d reached=%v, want 415 unreached", path,
				rec.Code, *reached)
		}
	}
}
