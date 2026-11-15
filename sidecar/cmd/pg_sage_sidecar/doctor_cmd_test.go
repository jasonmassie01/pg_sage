package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/doctor"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// No concurrent access tests: doctor is a one-shot command whose checks
// run sequentially on one connection by design.

func runDoctorForTest(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	t.Chdir(t.TempDir()) // no ./config.yaml leaks in
	t.Setenv("SAGE_CONFIG_PATH", "")
	var stdout, stderr bytes.Buffer
	code := runDoctorCommand(context.Background(), args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// Invalid input: a DSN that does not parse exits 2 naming the flag.
func TestDoctorCmdBadURLExits2(t *testing.T) {
	code, out, errOut := runDoctorForTest(t, "--pg-url", "not-a-url")
	if code != 2 || !strings.Contains(errOut, "parse --pg-url") || out != "" {
		t.Fatalf("exit %d stdout %q stderr %q, want 2 and \"parse --pg-url\"",
			code, out, errOut)
	}
}

// Invalid input: an unknown flag, a stray argument and no target exit 2.
func TestDoctorCmdUsageErrorsExit2(t *testing.T) {
	for _, args := range [][]string{
		{"--bogus"},
		{"--pg-url", "postgres://u@127.0.0.1:1/x", "extra"},
		{},
		{"--config", filepath.Join(os.TempDir(), "no-such-doctor-config.yaml")},
	} {
		code, out, errOut := runDoctorForTest(t, args...)
		if code != 2 || errOut == "" || out != "" {
			t.Errorf("args %q: exit %d stdout %q stderr %q, want 2 with a reason",
				args, code, out, errOut)
		}
	}
}

// Error propagation: an unreachable server exits 1 with one fail, and the
// password-bearing DSN never reaches stdout or stderr, in text or JSON.
func TestDoctorCmdNeverPrintsTheDSN(t *testing.T) {
	const password = "Doctor-Pa55word-Never-Print"
	dsn := "postgres://sage:" + password + "@127.0.0.1:1/orders?connect_timeout=2"
	for _, extra := range [][]string{nil, {"--json"}} {
		args := append([]string{"--pg-url", dsn}, extra...)
		code, out, errOut := runDoctorForTest(t, args...)
		if code != 1 {
			t.Fatalf("args %q: exit %d, want 1 (stdout %q stderr %q)", extra, code, out, errOut)
		}
		if strings.Contains(out+errOut, password) || strings.Contains(out+errOut, dsn) {
			t.Fatalf("args %q leaked the DSN: stdout %q stderr %q", extra, out, errOut)
		}
		if !strings.Contains(out, "cannot connect") {
			t.Fatalf("args %q: stdout %q does not say it cannot connect", extra, out)
		}
	}
}

// --json is the report: it parses, and with nothing reachable it has the
// golden's keys and check order.
func TestDoctorCmdJSONMatchesTheGoldenKeys(t *testing.T) {
	code, out, _ := runDoctorForTest(t, "--pg-url", "postgres://u@127.0.0.1:1/x?"+
		"connect_timeout=2", "--json")
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	assertGoldenShape(t, out)
}

// assertGoldenShape compares the report's keys and check IDs and titles,
// in order, with testdata/doctor_report.golden.json.
func assertGoldenShape(t *testing.T, out string) doctor.Report {
	t.Helper()
	golden, err := os.ReadFile(filepath.Join(doctorSourceDir, "testdata",
		"doctor_report.golden.json"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	var want, got map[string]any
	if err := json.Unmarshal(golden, &want); err != nil {
		t.Fatalf("golden: %v", err)
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, out)
	}
	if !reflect.DeepEqual(shapeOf(got), shapeOf(want)) {
		t.Fatalf("report shape\n got %v\nwant %v", shapeOf(got), shapeOf(want))
	}
	var r doctor.Report
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	return r
}

// shapeOf is the key set of the report, of its counts and of each check,
// plus each check's id and title in order.
func shapeOf(m map[string]any) []string {
	var out []string
	for _, k := range sortedKeys(m) {
		out = append(out, "top:"+k)
	}
	if counts, ok := m["counts"].(map[string]any); ok {
		for _, k := range sortedKeys(counts) {
			out = append(out, "counts:"+k)
		}
	}
	checks, _ := m["checks"].([]any)
	for _, raw := range checks {
		c, _ := raw.(map[string]any)
		out = append(out, "check:"+strings.Join(sortedKeys(c), ","),
			"id:"+toString(c["id"])+"|"+toString(c["title"]))
	}
	return out
}

func toString(v any) string {
	s, _ := v.(string)
	return s
}

// doctorSourceDir is this package's directory, read at package start,
// before runDoctorForTest moves each test into a temporary directory.
var doctorSourceDir = func() string {
	wd, _ := os.Getwd()
	return wd
}()

// Happy path (DB): a superuser on the test server exits 0 and the JSON
// keeps the golden's shape; the text output ends with the counts.
func TestDoctorCmdDBHappyPath(t *testing.T) {
	dsn := testdb.CreateDatabase(t, "doctor_cmd")
	code, out, errOut := runDoctorForTest(t, "--pg-url", dsn, "--json")
	if code != 0 {
		t.Fatalf("exit %d, want 0\nstdout %s\nstderr %s", code, out, errOut)
	}
	r := assertGoldenShape(t, out)
	if r.Counts.Fail != 0 || r.Provider != "SM" {
		t.Fatalf("report = %+v, want no fail on a self-managed server", r)
	}
	code, text, _ := runDoctorForTest(t, "--pg-url", dsn)
	if code != 0 || !strings.Contains(text, "PASS") || !strings.Contains(text, "0 fail") {
		t.Fatalf("text exit %d:\n%s", code, text)
	}
	// The fixture's password ("postgres") is also part of a check ID, so this
	// asserts the DSN and its user info; TestDoctorCmdNeverPrintsTheDSN
	// covers a distinctive password.
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse fixture dsn: %v", err)
	}
	if strings.Contains(out+text, dsn) || strings.Contains(out+text, u.User.String()+"@") {
		t.Fatalf("doctor printed the DSN or its user info:\n%s\n%s", out, text)
	}
}

// --send-test without a config has no channels to send through: the
// notification check says so instead of claiming a send.
func TestDoctorCmdSendTestWithoutConfig(t *testing.T) {
	code, out, _ := runDoctorForTest(t, "--pg-url", "postgres://u@127.0.0.1:1/x?"+
		"connect_timeout=2", "--json", "--send-test")
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	var r doctor.Report
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("decode: %v", err)
	}
	c, _ := r.Find(doctor.IDNotification)
	if c.Status != doctor.StatusWarn || !strings.Contains(c.Detail, "no notification") {
		t.Fatalf("notification = %+v, want warn \"no notification channels\"", c)
	}
}

// Doctor's host classification agrees with the sidecar's hosted-provider
// detection for every hosted endpoint shape the sidecar knows.
func TestDoctorProviderAgreesWithHostedProvider(t *testing.T) {
	want := map[string]string{"neon": "neon", "supabase": "supabase", "azure": "azure",
		"azure-cosmos": "azure", "lakebase": "lakebase"}
	for _, host := range []string{
		"ep-x-1.us-east-2.aws.neon.tech", "db.abc.supabase.co",
		"aws-0-us-east-1.pooler.supabase.com", "a.postgres.database.azure.com",
		"c-x.abc.postgres.cosmos.azure.com", "i-1.database.us-west-2.cloud.databricks.com",
		"i-1.database.eastus.azuredatabricks.net", "db.internal",
	} {
		sidecar := hostedProviderFromHost(host)
		got := doctor.DetectProvider(doctor.ProviderFacts{Host: host}).Provider
		if sidecar == "" {
			if got != "unknown" {
				t.Errorf("%s: doctor says %s, the sidecar recognises nothing", host, got)
			}
			continue
		}
		if got != want[sidecar] {
			t.Errorf("%s: doctor says %s, the sidecar says %s", host, got, sidecar)
		}
	}
}
