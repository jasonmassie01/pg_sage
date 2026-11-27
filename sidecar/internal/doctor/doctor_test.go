package doctor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// No concurrent access tests: Run is sequential by design (one connection,
// one check at a time) and keeps no state between runs.

var reportOrder = []string{IDConnection, IDVersion, IDRoleGrants, IDExtensions,
	IDProvider, IDQueryText, IDModel, IDNotification, IDMetaDB, IDClockSkew}

func assertOrder(t *testing.T, r Report) {
	t.Helper()
	if len(r.Checks) != len(reportOrder) {
		t.Fatalf("got %d checks, want %d: %+v", len(r.Checks), len(reportOrder), r.Checks)
	}
	for i, id := range reportOrder {
		if r.Checks[i].ID != id || r.Checks[i].Title == "" {
			t.Fatalf("check %d = %q (title %q), want %q with a title",
				i, r.Checks[i].ID, r.Checks[i].Title, id)
		}
	}
}

func mustCheck(t *testing.T, r Report, id string) Check {
	t.Helper()
	c, ok := r.Find(id)
	if !ok {
		t.Fatalf("report has no %s check: %+v", id, r.Checks)
	}
	return c
}

// unreachablePool is a lazily connecting pool to a port nothing listens on.
func unreachablePool(t *testing.T, password string) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig("postgres://doctor:" + password +
		"@127.0.0.1:1/postgres?connect_timeout=2&sslmode=disable")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// Nil/empty: a run with nothing at all fails the connection check once,
// and leaves every other check unknown with its reason, not a fail.
func TestRunNilInputsFailsOnlyTheConnection(t *testing.T) {
	r := Run(context.Background(), Inputs{})
	assertOrder(t, r)
	if c := mustCheck(t, r, IDConnection); c.Status != StatusFail || c.Fix == "" {
		t.Fatalf("connection = %+v, want fail with a fix", c)
	}
	if r.Counts != (Counts{Fail: 1, Unknown: 9}) || r.ExitCode() != 1 {
		t.Fatalf("counts = %+v exit %d, want 1 fail 9 unknown, exit 1", r.Counts, r.ExitCode())
	}
	if r.Provider != "unknown" {
		t.Fatalf("provider = %q, want unknown", r.Provider)
	}
	model := mustCheck(t, r, IDModel)
	if model.Status != StatusUnknown || !strings.Contains(model.Detail, "no model configured") {
		t.Fatalf("model = %+v, want unknown \"no model configured\"", model)
	}
	meta := mustCheck(t, r, IDMetaDB)
	if meta.Status != StatusUnknown || !strings.Contains(meta.Detail, "not configured") {
		t.Fatalf("meta = %+v, want unknown \"not configured\"", meta)
	}
	notify := mustCheck(t, r, IDNotification)
	if notify.Status != StatusUnknown || !strings.Contains(notify.Detail, "--send-test") {
		t.Fatalf("notification = %+v, want unknown naming --send-test", notify)
	}
}

// Error propagation: an unreachable host is one fail and the rest of the
// database checks unknown "no connection"; the password never appears.
func TestRunUnreachableHostIsOneFail(t *testing.T) {
	const password = "s3cret-doctor-pw"
	r := Run(context.Background(), Inputs{Target: unreachablePool(t, password),
		Secrets: []string{password}})
	conn := mustCheck(t, r, IDConnection)
	if conn.Status != StatusFail || !strings.Contains(conn.Detail, "cannot connect") {
		t.Fatalf("connection = %+v, want fail \"cannot connect\"", conn)
	}
	for _, id := range []string{IDVersion, IDRoleGrants, IDExtensions, IDProvider,
		IDQueryText, IDClockSkew} {
		c := mustCheck(t, r, id)
		if c.Status != StatusUnknown || !strings.Contains(c.Detail, "no connection") {
			t.Fatalf("%s = %+v, want unknown \"no connection\"", id, c)
		}
	}
	if r.Counts.Fail != 1 || r.ExitCode() != 1 {
		t.Fatalf("counts = %+v, want exactly one fail", r.Counts)
	}
	for _, c := range r.Checks {
		if strings.Contains(c.Detail+c.Fix, password) {
			t.Fatalf("%s leaks the password: %+v", c.ID, c)
		}
	}
}

// Boundary: PostgreSQL 13 fails, 14 through 18 pass, a newer major warns.
func TestVersionStatusBoundaries(t *testing.T) {
	cases := []struct {
		num  int
		want Status
		text string
	}{
		{130012, StatusFail, "PostgreSQL 14 or later"},
		{139999, StatusFail, "PostgreSQL 14 or later"},
		{140000, StatusPass, ""},
		{170004, StatusPass, ""},
		{189999, StatusPass, ""},
		{190000, StatusWarn, "14 to 18"},
		{0, StatusUnknown, "could not read"},
	}
	for _, tc := range cases {
		c := versionCheck(tc.num, "x")
		if c.Status != tc.want || !strings.Contains(c.Detail+c.Fix, tc.text) {
			t.Errorf("version %d = %+v, want %s containing %q", tc.num, c, tc.want, tc.text)
		}
	}
}

// Boundary: a skew of exactly 2 s passes, 2.001 s warns, 30 s warns,
// 30.001 s fails, in either direction.
func TestSkewStatusBoundaries(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want Status
	}{
		{0, StatusPass},
		{2 * time.Second, StatusPass},
		{-2 * time.Second, StatusPass},
		{2001 * time.Millisecond, StatusWarn},
		{-2001 * time.Millisecond, StatusWarn},
		{30 * time.Second, StatusWarn},
		{30001 * time.Millisecond, StatusFail},
		{-30001 * time.Millisecond, StatusFail},
	}
	for _, tc := range cases {
		if got := skewStatus(tc.d); got != tc.want {
			t.Errorf("skewStatus(%s) = %s, want %s", tc.d, got, tc.want)
		}
	}
}

// Nil/empty and state: no test requested, no channels, sent, and failed.
func TestNotifyCheckStates(t *testing.T) {
	ctx := context.Background()
	if c := notifyCheck(ctx, nil); c.Status != StatusUnknown {
		t.Fatalf("nil notify = %+v, want unknown", c)
	}
	none := notifyCheck(ctx, func(context.Context) (int, error) { return 0, nil })
	if none.Status != StatusWarn || none.Fix == "" {
		t.Fatalf("no channels = %+v, want warn with a fix", none)
	}
	sent := notifyCheck(ctx, func(context.Context) (int, error) { return 2, nil })
	if sent.Status != StatusPass || !strings.Contains(sent.Detail, "2 channel") {
		t.Fatalf("sent = %+v, want pass naming 2 channels", sent)
	}
	failed := notifyCheck(ctx, func(context.Context) (int, error) {
		return 1, errors.New("slack: delivery failed: status 404")
	})
	if failed.Status != StatusFail || !strings.Contains(failed.Detail, "status 404") {
		t.Fatalf("failed = %+v, want fail carrying the cause", failed)
	}
}

// Error propagation: a meta database that could not be opened fails.
func TestMetaCheckOpenError(t *testing.T) {
	c := metaCheck(context.Background(), nil, errors.New("parse meta_db: bad"), nil)
	if c.Status != StatusFail || !strings.Contains(c.Detail, "parse meta_db") {
		t.Fatalf("meta = %+v, want fail carrying the open error", c)
	}
}

// Invalid input: passwords are masked in URLs, key=value DSNs and given
// secrets, and an empty secret masks nothing.
func TestMaskHidesPasswords(t *testing.T) {
	cases := map[string]string{
		"dial postgres://sage:hunter2@db:5432/x failed":     "postgres://sage:****@db:5432/x",
		"host=db password=hunter2 user=sage":                "password=****",
		"host=db password='hunter 2' user=sage":             "password=****",
		"meta_db postgresql://u:p%40ss@h/x?sslmode=require": "postgresql://u:****@h/x",
		"the key hunter2 was refused":                       "the key **** was refused",
	}
	for in, want := range cases {
		got := Mask(in, []string{"hunter2", ""})
		if !strings.Contains(got, want) || strings.Contains(got, "hunter2") {
			t.Errorf("Mask(%q) = %q, want it to contain %q", in, got, want)
		}
	}
	if got := Mask("nothing secret", nil); got != "nothing secret" {
		t.Fatalf("mask changed plain text: %q", got)
	}
}

// Count puts an unrecognised status with unknown, never with pass.
func TestCountUnrecognisedStatusIsUnknown(t *testing.T) {
	c := Count([]Check{{Status: StatusPass}, {Status: "bogus"}, {Status: StatusWarn}})
	if c != (Counts{Pass: 1, Warn: 1, Unknown: 1}) {
		t.Fatalf("counts = %+v", c)
	}
	if (Report{Counts: Counts{Warn: 3}}).ExitCode() != 0 {
		t.Fatal("warnings alone must exit 0")
	}
}

// Masking edge cases: an unencoded '@' in a URL password and an escaped
// quote in a key=value password leave no part of the password behind.
func TestMaskPasswordEdgeCases(t *testing.T) {
	for _, tc := range []struct{ in, leak string }{
		{"dial postgres://sage:p@ss-tail@db.internal/app failed", "ss-tail"},
		{`host=db password='it\'s-tail' user=sage`, "s-tail"},
	} {
		got := Mask(tc.in, nil)
		if strings.Contains(got, tc.leak) || !strings.Contains(got, maskedPassword) {
			t.Errorf("Mask(%q) = %q, leaks %q", tc.in, got, tc.leak)
		}
	}
	got := Mask("postgres://sage:p@ss@db/app", []string{"p@ss"})
	if strings.Contains(got, "ss@db") || !strings.Contains(got, "@db/app") {
		t.Errorf("Mask with the secret = %q, want the host kept and no password", got)
	}
}
