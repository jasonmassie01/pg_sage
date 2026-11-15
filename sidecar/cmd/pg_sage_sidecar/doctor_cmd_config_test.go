package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/pg-sage/sidecar/internal/doctor"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// No concurrent access tests: one doctor run per case, sequential checks.

const doctorConfigKey = "sk-doctor-config-key-987654321"

// writeDoctorConfig writes a sidecar config with an llm: block, a meta_db
// and one webhook, all pointing at local test doubles.
func writeDoctorConfig(t *testing.T, metaDSN, modelURL, hookURL string) string {
	t.Helper()
	body := "llm:\n  enabled: true\n  endpoint: " + modelURL + "\n  api_key: " +
		doctorConfigKey + "\n  model: doctor-model\n  timeout_seconds: 5\n" +
		"meta_db: " + metaDSN + "\n" +
		"notification_policy:\n  allow_private_targets: true\n" +
		"alerting:\n  enabled: true\n  routes:\n    - severity: critical\n" +
		"      channels: [hook]\n  webhooks:\n    - name: hook\n      url: " +
		hookURL + "\n"
	path := filepath.Join(t.TempDir(), "sage.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func modelDouble(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"OK"}}],` +
			`"usage":{"total_tokens":3}}`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// hookDouble counts posts and answers each with status.
func hookDouble(t *testing.T, status int) (string, *atomic.Int32) {
	t.Helper()
	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		posts.Add(1)
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/hook/secret-token-in-path", &posts
}

// Integration: --config adds the model probe, the meta database and,
// with --send-test, one test through the webhook; without --send-test
// nothing is posted. No secret from the config reaches the output.
func TestDoctorCmdDBConfigChecks(t *testing.T) {
	dsn := testdb.CreateDatabase(t, "doctor_cfg_target")
	meta := testdb.CreateDatabase(t, "doctor_cfg_meta")
	hook, posts := hookDouble(t, http.StatusOK)
	cfgPath := writeDoctorConfig(t, meta, modelDouble(t), hook)

	code, out, errOut := runDoctorForTest(t, "--pg-url", dsn, "--config", cfgPath, "--json")
	r := decodeDoctor(t, out, errOut)
	if code != 0 || posts.Load() != 0 {
		t.Fatalf("exit %d with %d posts, want 0 and none: %s", code, posts.Load(), errOut)
	}
	wantStatus(t, r, doctor.IDModel, doctor.StatusPass)
	wantStatus(t, r, doctor.IDMetaDB, doctor.StatusWarn) // reachable, not bootstrapped
	wantStatus(t, r, doctor.IDNotification, doctor.StatusUnknown)

	code, out, errOut = runDoctorForTest(t, "--pg-url", dsn, "--config", cfgPath, "--json",
		"--send-test")
	r = decodeDoctor(t, out, errOut)
	if code != 0 || posts.Load() != 1 {
		t.Fatalf("exit %d with %d posts, want 0 and one: %s", code, posts.Load(), errOut)
	}
	wantStatus(t, r, doctor.IDNotification, doctor.StatusPass)
	for _, secret := range []string{doctorConfigKey, "secret-token-in-path"} {
		if strings.Contains(out+errOut, secret) {
			t.Fatalf("output carries %q:\n%s\n%s", secret, out, errOut)
		}
	}
}

// Error propagation: a webhook that answers 500 fails the notification
// check (exit 1), and the detail hides the webhook URL.
func TestDoctorCmdDBSendTestFailure(t *testing.T) {
	dsn := testdb.CreateDatabase(t, "doctor_cfg_hookfail")
	hook, posts := hookDouble(t, http.StatusInternalServerError)
	cfgPath := writeDoctorConfig(t, dsn, modelDouble(t), hook)
	code, out, errOut := runDoctorForTest(t, "--pg-url", dsn, "--config", cfgPath, "--json",
		"--send-test")
	r := decodeDoctor(t, out, errOut)
	c := wantStatus(t, r, doctor.IDNotification, doctor.StatusFail)
	if code != 1 || posts.Load() == 0 || strings.Contains(c.Detail, "secret-token-in-path") {
		t.Fatalf("exit %d posts %d detail %q, want 1, a post and no URL", code, posts.Load(),
			c.Detail)
	}
}

func decodeDoctor(t *testing.T, out, errOut string) doctor.Report {
	t.Helper()
	var r doctor.Report
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("stdout is not a report: %v\nstdout %s\nstderr %s", err, out, errOut)
	}
	return r
}

func wantStatus(t *testing.T, r doctor.Report, id string, want doctor.Status) doctor.Check {
	t.Helper()
	c, ok := r.Find(id)
	if !ok || c.Status != want {
		t.Fatalf("%s = %+v, want %s", id, c, want)
	}
	return c
}
