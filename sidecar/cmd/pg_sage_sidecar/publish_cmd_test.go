package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/evidence"
	"github.com/pg-sage/sidecar/internal/outcomes"
	"github.com/pg-sage/sidecar/internal/publish"
	"github.com/pg-sage/sidecar/internal/verify/backtest"
)

// pub-01: pg_sage publish outcomes, without a database (fake sources).
// No concurrency test: a single-shot command over one read snapshot.

var publishNow = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

type fakePublishSource struct {
	res     outcomes.Result
	err     error
	bt      backtest.Counts
	btFound bool
	btErr   error
	queries *[]outcomes.Query
	closed  *int
}

func (f fakePublishSource) ReadOutcomes(_ context.Context, q outcomes.Query) (outcomes.Result,
	error) {
	if f.queries != nil {
		*f.queries = append(*f.queries, q)
	}
	return f.res, f.err
}

func (f fakePublishSource) LatestBacktest(context.Context) (backtest.Counts, bool, error) {
	return f.bt, f.btFound, f.btErr
}

func (f fakePublishSource) Close() {
	if f.closed != nil {
		*f.closed++
	}
}

func publishKeyPEM(t *testing.T) (string, ed25519.PublicKey) {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	text, err := evidence.MarshalSigningKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return text, pub
}

func publishTestDeps(t *testing.T, src fakePublishSource, openErr error) (publishDeps,
	ed25519.PublicKey) {
	t.Helper()
	pemText, pub := publishKeyPEM(t)
	return publishDeps{
		getenv: func(string) string { return "" },
		lookupSecret: func(name string) (string, error) {
			if name == defaultPublishKeyEnv {
				return pemText, nil
			}
			return "", nil
		},
		open: func(context.Context, string) (publishSource, error) {
			if openErr != nil {
				return nil, openErr
			}
			return src, nil
		},
		now: func() time.Time { return publishNow },
	}, pub
}

func runPublish(t *testing.T, deps publishDeps, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := runPublishCommand(context.Background(), args, deps, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestPublishOutcomes_WritesASignedBundleOverTheWindow(t *testing.T) {
	var queries []outcomes.Query
	closed := 0
	src := fakePublishSource{res: outcomes.Result{Rows: publish.SampleRows()},
		bt: backtest.Counts{Placebos: 300, FalsePositives: 6}, btFound: true,
		queries: &queries, closed: &closed}
	deps, pub := publishTestDeps(t, src, nil)
	out := filepath.Join(t.TempDir(), "bundle")
	code, stdout, stderr := runPublish(t, deps, "outcomes", "--since", "90d", "--out", out,
		"--dsn", "postgres://u@h/app_one", "--dsn", "postgres://u@h/app_two")
	if code != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr)
	}
	if len(queries) != 2 || queries[0].Since != 90*24*time.Hour ||
		!queries[0].Now.Equal(publishNow) || queries[0].Limit != outcomes.MaxLimit {
		t.Fatalf("ledger queries %+v, want two 90-day reads at the command's clock", queries)
	}
	if closed != 2 {
		t.Fatalf("%d sources closed, want 2", closed)
	}
	pack, err := os.ReadFile(filepath.Join(out, publish.PackName))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := evidence.Verify(bytes.NewReader(pack), pub); err != nil {
		t.Fatalf("the bundle does not verify: %v", err)
	}
	raw, _ := os.ReadFile(filepath.Join(out, publish.JSONName))
	for _, want := range []string{`"changes": 80`, `"databases": 2`, `"backtests": 2`,
		`"since": "90d"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("outcomes.json lacks %s:\n%s", want, raw)
		}
	}
	if strings.Contains(string(raw), "app_one") || !strings.Contains(stdout, out) {
		t.Fatalf("stdout %q; outcomes.json names a database: %v", stdout,
			strings.Contains(string(raw), "app_one"))
	}
}

func TestPublishOutcomes_UsageErrorsExitTwoAndWriteNothing(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "a-file")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "bundle")
	dsn := "postgres://u@h/app"
	cases := map[string][]string{
		"no subcommand":   {},
		"wrong verb":      {"incidents", "--out", out, "--dsn", dsn},
		"zero days":       {"outcomes", "--since", "0d", "--out", out, "--dsn", dsn},
		"unparseable":     {"outcomes", "--since", "ninety", "--out", out, "--dsn", dsn},
		"past retention":  {"outcomes", "--since", "401d", "--out", out, "--dsn", dsn},
		"no out":          {"outcomes", "--dsn", dsn},
		"out is a file":   {"outcomes", "--out", file, "--dsn", dsn},
		"no dsn":          {"outcomes", "--out", out},
		"stray argument":  {"outcomes", "--out", out, "--dsn", dsn, "extra"},
		"unknown flag":    {"outcomes", "--out", out, "--dsn", dsn, "--nope"},
		"sample with dsn": {"outcomes", "--sample", "--out", out, "--dsn", dsn},
	}
	for name, args := range cases {
		deps, _ := publishTestDeps(t, fakePublishSource{}, nil)
		code, _, stderr := runPublish(t, deps, args...)
		if code != 2 || !strings.Contains(stderr, "usage: pg_sage publish outcomes") {
			t.Errorf("%s: exit %d, stderr %q; want 2 with the usage", name, code, stderr)
		}
	}
	assertDirHolds(t, dir, "a-file")
}

func TestPublishOutcomes_AMissingOrBadKeyExitsTwo(t *testing.T) {
	out := filepath.Join(t.TempDir(), "bundle")
	for name, lookup := range map[string]func(string) (string, error){
		"unset":      func(string) (string, error) { return "", nil },
		"not pem":    func(string) (string, error) { return "not a key", nil },
		"lookup err": func(string) (string, error) { return "", errors.New("both set") },
	} {
		deps, _ := publishTestDeps(t, fakePublishSource{}, nil)
		deps.lookupSecret = lookup
		code, _, stderr := runPublish(t, deps, "outcomes", "--out", out, "--dsn",
			"postgres://u@h/app")
		if code != 2 || !strings.Contains(stderr, defaultPublishKeyEnv) {
			t.Errorf("%s: exit %d, stderr %q; want 2 naming %s", name, code, stderr,
				defaultPublishKeyEnv)
		}
		if strings.Contains(stderr, "not a key") {
			t.Errorf("%s: stderr echoes the key material: %q", name, stderr)
		}
	}
	if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused run left %s: %v", out, err)
	}
}

func TestPublishOutcomes_ReadFailuresExitOneWithNoPartialBundle(t *testing.T) {
	boom := errors.New("read outcomes: app: connection refused")
	cases := map[string]struct {
		src     fakePublishSource
		openErr error
		want    string
	}{
		"connect":  {openErr: errors.New("dial tcp: refused"), want: "connect"},
		"ledger":   {src: fakePublishSource{err: boom}, want: "connection refused"},
		"backtest": {src: fakePublishSource{btErr: errors.New("no table")}, want: "no table"},
		"truncated": {src: fakePublishSource{res: outcomes.Result{Truncated: true}},
			want: "more than"},
	}
	for name, c := range cases {
		dir := t.TempDir()
		deps, _ := publishTestDeps(t, c.src, c.openErr)
		code, _, stderr := runPublish(t, deps, "outcomes", "--out",
			filepath.Join(dir, "bundle"), "--dsn", "postgres://u:pw@h/app")
		if code != 1 || !strings.Contains(stderr, c.want) {
			t.Errorf("%s: exit %d, stderr %q; want 1 naming %q", name, code, stderr, c.want)
		}
		if strings.Contains(stderr, "pw@") {
			t.Errorf("%s: stderr prints the DSN: %q", name, stderr)
		}
		assertDirHolds(t, dir)
	}
}

func TestPublishOutcomes_EmptyLedgerSaysNoChanges(t *testing.T) {
	deps, _ := publishTestDeps(t, fakePublishSource{}, nil)
	out := filepath.Join(t.TempDir(), "bundle")
	code, _, stderr := runPublish(t, deps, "outcomes", "--out", out, "--dsn",
		"postgres://u@h/app")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	raw, _ := os.ReadFile(filepath.Join(out, publish.JSONName))
	if !strings.Contains(string(raw), publish.NoChangesStatement) ||
		!strings.Contains(string(raw), `"totals": null`) {
		t.Fatalf("empty ledger bundle:\n%s", raw)
	}
}

func TestPublishOutcomes_SampleNeedsNoDatabaseOrKey(t *testing.T) {
	deps, _ := publishTestDeps(t, fakePublishSource{}, errors.New("must not connect"))
	deps.lookupSecret = func(string) (string, error) { return "", errors.New("no key read") }
	out := filepath.Join(t.TempDir(), "sample")
	if code, _, stderr := runPublish(t, deps, "outcomes", "--sample", "--out", out); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	pack, _ := os.ReadFile(filepath.Join(out, publish.PackName))
	pub := publish.SampleKey().Public().(ed25519.PublicKey)
	if _, err := evidence.Verify(bytes.NewReader(pack), pub); err != nil {
		t.Fatalf("the sample does not verify with the sample key: %v", err)
	}
}

func assertDirHolds(t *testing.T, dir string, names ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(names) {
		t.Fatalf("%s holds %d entries %v, want %v", dir, len(entries), entries, names)
	}
	for i, e := range entries {
		if e.Name() != names[i] {
			t.Fatalf("%s holds %v, want %v", dir, entries, names)
		}
	}
}

// pub-01 review: the same --dsn twice would count its changes twice in a
// public number, so it is a usage error, and the error never prints the
// DSN (which can hold a password).
func TestPublishOutcomes_ARepeatedDSNExitsTwo(t *testing.T) {
	dsn := "postgres://u:secretpw@h/app"
	deps, _ := publishTestDeps(t, fakePublishSource{}, nil)
	dir := t.TempDir()
	code, _, stderr := runPublish(t, deps, "outcomes", "--out", filepath.Join(dir, "b"),
		"--dsn", dsn, "--dsn", dsn)
	if code != 2 || !strings.Contains(stderr, "repeated") {
		t.Fatalf("exit %d, stderr %q; want 2 naming the repeated --dsn", code, stderr)
	}
	if strings.Contains(stderr, "secretpw") {
		t.Fatalf("stderr prints the DSN: %q", stderr)
	}
	assertDirHolds(t, dir)
	code, _, stderr = runPublish(t, deps, "outcomes", "--out", filepath.Join(dir, "b"),
		"--dsn", dsn, "--dsn", "postgres://u@h/other")
	if code != 0 {
		t.Fatalf("two different databases: exit %d: %s", code, stderr)
	}
}
