package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/databricksauth"
	"github.com/pg-sage/sidecar/internal/sagepool"
	"github.com/pg-sage/sidecar/internal/selfmonitor"
	"github.com/pg-sage/sidecar/internal/sre"
)

// The one-shot commands open pg_sage's own sessions: sagepool for ledger,
// publish, bench export-replay, verifier-backtest and history, and
// selfmonitor.ConfigurePool under doctor's own settings. The tag goes in
// through the protocol frontend's writer, so the network connection pgx
// holds stays the *tls.Conn that SCRAM channel binding needs. These tests
// need no database (cli_pool_db_test.go has the live ones). No concurrent
// access tests here: each case opens one lazy pool or runs one command;
// concurrent runs are TestCLIPoolDB_LedgerSessionsArePgSagesOwn's.

// cliPoolSecret is a password no output may carry.
const cliPoolSecret = "Cli-Pool-Pa55word-Never-Print"

// cliPoolBadDSN does not parse (its port is not a number).
const cliPoolBadDSN = "postgres://sage:" + cliPoolSecret + "@127.0.0.1:notaport/app"

// cliPoolHalfOne and cliPoolHalfTwo are the pieces of a password split at
// '@', ':' or '/'. pgx's own parse error printed one of them: its redaction
// keeps the piece after '@' or before ':', and the url error it wraps
// quotes the piece before '/' as an invalid port.
const cliPoolHalfOne, cliPoolHalfTwo = "Cli-Pool-Half-One", "Cli-Pool-Half-Two"

// cliPoolSplitDSN is a DSN whose password is cliPoolHalfOne, sep,
// cliPoolHalfTwo, on port (a '/' alone breaks the parse, so 5432 does).
func cliPoolSplitDSN(sep, port string) string {
	return "postgres://sage:" + cliPoolHalfOne + sep + cliPoolHalfTwo + "@127.0.0.1:" + port +
		"/app"
}

// cliPoolLakeHost names a Lakebase endpoint while cliPoolLakebaseEnv maps
// every host to one.
const cliPoolLakeHost = "ep-x.database.us-east-1.cloud.databricks.com"

// cliPoolPins are the session pins as the tagger sends them; values are
// left out, since pg_stat_statements may normalize them.
var cliPoolPins = []string{
	"SET " + selfmonitor.StatementTag + " SESSION default_transaction_isolation",
	"SET " + selfmonitor.StatementTag + " SESSION search_path",
}

// cliPoolMarkers is the pins plus a command's own statements.
func cliPoolMarkers(extra ...string) []string {
	return append(slices.Clone(cliPoolPins), extra...)
}

// cliPoolTagged reports whether text, as pg_stat_statements or
// pg_stat_activity shows it, carries a pg_sage tag comment right after its
// first keyword (or opening parenthesis), past any leading white space and
// comments: where the tagger puts it, since PostgreSQL 18's
// pg_stat_statements drops a comment in front of the first token.
func cliPoolTagged(text string) bool {
	i, ok := cliPoolSkipLeading(text)
	if !ok {
		return false
	}
	j := i
	for j < len(text) && 'a' <= text[j]|0x20 && text[j]|0x20 <= 'z' {
		j++
	}
	if j == i && j < len(text) && text[j] == '(' {
		j++
	}
	if j == i {
		return false
	}
	rest := strings.TrimLeft(text[j:], " \t\r\n")
	end := strings.Index(rest, "*/")
	return strings.HasPrefix(rest, "/*") && end >= 2 && selfmonitor.IsTagged(rest[:end+2])
}

// cliPoolSkipLeading is the offset of text's first token past white space
// and comments; ok is false for an unterminated block comment.
func cliPoolSkipLeading(text string) (int, bool) {
	i := 0
	for i < len(text) {
		switch {
		case strings.IndexByte(" \t\r\n\f", text[i]) >= 0:
			i++
		case strings.HasPrefix(text[i:], "--"):
			end := strings.IndexByte(text[i:], '\n')
			if end < 0 {
				return len(text), true
			}
			i += end + 1
		case strings.HasPrefix(text[i:], "/*"):
			end := strings.Index(text[i+2:], "*/")
			if end < 0 {
				return 0, false
			}
			i += end + 4
		default:
			return i, true
		}
	}
	return i, true
}

// cliPoolProblems lists each text not tagged where the tagger puts it and
// each marker no text contains.
func cliPoolProblems(texts, markers []string) []string {
	var problems []string
	for _, text := range texts {
		if !cliPoolTagged(text) {
			problems = append(problems, fmt.Sprintf("statement recorded without the "+
				"tag right after its first keyword: %q", text))
		}
	}
	for _, m := range markers {
		if !slices.ContainsFunc(texts, func(s string) bool { return strings.Contains(s, m) }) {
			problems = append(problems, fmt.Sprintf("no statement containing %q recorded "+
				"(the SET pins need pg_stat_statements.track_utility on)", m))
		}
	}
	return problems
}

// The checker itself: the tagger's placement passes; a tag in front of the
// first keyword (the old placement, which PostgreSQL 18 drops), a later
// tag, another comment and unreadable text do not.
func TestCLIPool_TaggedRecognizesOnlyTheTaggersPlacement(t *testing.T) {
	tag := selfmonitor.StatementTag
	for text, want := range map[string]bool{
		"SELECT " + tag + " 1":                      true,
		"select " + tag + "\n  x FROM t":            true,
		"SELECT /* pg_sage sre:lock_graph v1 */ 1":  true,
		"-- note\nCREATE " + tag + " TABLE t ()":    true,
		"/* other */ SET " + tag + " SESSION a = 1": true,
		"( " + tag + " SELECT 1)":                   true,
		"begin " + tag:                              true,
		tag + " SELECT 1":                           false,
		"SELECT 1":                                  false,
		"SELECT 1 " + tag:                           false,
		"SELECT /* other */ " + tag + " 1":          false,
		"SELECT /* pg_sage":                         false,
		"/* pg_sage */":                             false,
		"/* unterminated SELECT 1":                  false,
		"-- only a comment":                         false,
		"   ":                                       false,
		"":                                          false,
	} {
		if got := cliPoolTagged(text); got != want {
			t.Errorf("cliPoolTagged(%q) = %v, want %v", text, got, want)
		}
	}
}

// Nil, empty and partial inputs: no texts misses every marker; an untagged
// text is named; a tagged text holding every marker is no problem.
func TestCLIPool_ProblemsNamesUntaggedTextsAndMissingMarkers(t *testing.T) {
	if got := cliPoolProblems(nil, nil); got != nil {
		t.Fatalf("no texts, no markers = %q, want none", got)
	}
	if got := cliPoolProblems(nil, cliPoolPins); len(got) != 2 ||
		!strings.Contains(got[0], "SESSION default_transaction_isolation") ||
		!strings.Contains(got[1], "SESSION search_path") {
		t.Fatalf("no texts = %q, want both pins missing", got)
	}
	texts := []string{cliPoolPins[0] + " = $1", cliPoolPins[1] + " = $1, $2", "SELECT 1"}
	got := cliPoolProblems(texts, cliPoolPins)
	if len(got) != 1 || !strings.Contains(got[0], `"SELECT 1"`) {
		t.Fatalf("one untagged text = %q, want only it named", got)
	}
	if got := cliPoolProblems(texts[:2], cliPoolMarkers()); got != nil {
		t.Fatalf("tagged pins = %q, want none", got)
	}
}

// State: the verifier-backtest default open (sagepool.New) and doctor's
// pool (ConfigurePool, then doctor's parameters) send each statement with
// the tag right after its first keyword through the frontend, never wrap
// the network connection (a wrapped one is no *tls.Conn, so pgx refuses
// channel_binding=require), pin through AfterConnect and keep their names:
// doctor's application_name, set after the pins, stays pg_sage_doctor.
func TestCLIPool_OpenPathsTagThroughTheFrontend(t *testing.T) {
	t.Setenv("SAGE_LAKEBASE_ENDPOINT", "")
	t.Setenv("SAGE_LAKEBASE_ENDPOINTS", "")
	ctx := context.Background()
	const dsn = "postgres://sage:pw@127.0.0.1:1/app?sslmode=disable"
	backtestPool, err := defaultBacktestDeps().open(ctx, dsn)
	if err != nil {
		t.Fatalf("verifier-backtest open: %v", err)
	}
	defer backtestPool.Close()
	doctorPool, _, err := openDoctorPool(ctx, dsn)
	if err != nil {
		t.Fatalf("doctor open: %v", err)
	}
	defer doctorPool.Close()
	bounds := strings.Replace(publishBoundsSQL, "SELECT", "SELECT "+selfmonitor.StatementTag, 1)
	pin := cliPoolPins[1] + " = pg_catalog, pg_temp"
	want := []string{bounds, bounds, pin, pin}
	for _, c := range []struct {
		name, app string
		pool      *pgxpool.Pool
	}{
		{"verifier-backtest", selfmonitor.ApplicationName, backtestPool},
		{"doctor", "pg_sage_doctor", doctorPool},
	} {
		cfg := c.pool.Config().ConnConfig
		if got := cfg.RuntimeParams["application_name"]; got != c.app {
			t.Errorf("%s: application_name %q, want %q", c.name, got, c.app)
		}
		if cfg.AfterNetConnect != nil || cfg.AfterConnect == nil {
			t.Errorf("%s: AfterNetConnect set %v, AfterConnect set %v; want the network "+
				"connection unwrapped and the pins installed", c.name,
				cfg.AfterNetConnect != nil, cfg.AfterConnect != nil)
		}
		sent := cliPoolSent(t, &cfg.Config, publishBoundsSQL, selfmonitor.PinSearchPathSQL)
		if !slices.Equal(sent, want) || !cliPoolTagged(sent[0]) || !cliPoolTagged(sent[2]) {
			t.Errorf("%s sent %q, want %q", c.name, sent, want)
		}
	}
}

// cliPoolLakebaseEnv maps hosts to a Lakebase endpoint (endpoint: every
// host; endpoints: a JSON host map), with fake workspace credentials on a
// local address so a regression past the TLS check reaches no workspace.
func cliPoolLakebaseEnv(t *testing.T, endpoint, endpoints string) {
	t.Helper()
	t.Setenv("DATABRICKS_CONFIG_FILE", t.TempDir()+"/absent.cfg")
	t.Setenv("DATABRICKS_CONFIG_PROFILE", "")
	t.Setenv("DATABRICKS_HOST", "https://127.0.0.1:1")
	t.Setenv("DATABRICKS_AUTH_TYPE", "pat")
	t.Setenv("DATABRICKS_TOKEN", "test-not-a-real-token")
	t.Setenv("SAGE_LAKEBASE_ENDPOINT", endpoint)
	t.Setenv("SAGE_LAKEBASE_ENDPOINTS", endpoints)
	// sslrootcert=system makes pgx verify-full, which the refusal admits.
	t.Setenv("PGSSLROOTCERT", "")
}

// cliPoolLakeDSN is a Lakebase DSN with the given sslmode.
func cliPoolLakeDSN(sslmode string) string {
	return "postgres://sage:" + cliPoolSecret + "@" + cliPoolLakeHost + ":5432/app?sslmode=" +
		sslmode
}

// Invalid input: every sagepool command refuses a Lakebase host whose TLS
// does not verify it, with ErrUnverifiedTLS unchanged, before a credential
// exists. Doctor is not among them: its open path has no Lakebase OAuth.
func TestCLIPool_LakebaseUnverifiedTLSRefusedByEveryOpenPath(t *testing.T) {
	cliPoolLakebaseEnv(t, "projects/p/branches/b/endpoints/e", "")
	ctx := context.Background()
	dsn, want := cliPoolLakeDSN("require"), databricksauth.ErrUnverifiedTLS.Error()
	code, out, errOut := runLedgerCLI(t, map[string]string{"SAGE_META_DB": dsn},
		"--database", "orders")
	if code != 2 || out != "" || errOut != "ledger: connect to orders: "+want+"\n" {
		t.Errorf("ledger: exit %d stdout %q stderr %q, want 2 and the refusal", code, out,
			errOut)
	}
	if src, err := openPublishSource(ctx, dsn); src != nil ||
		!errors.Is(err, databricksauth.ErrUnverifiedTLS) {
		t.Errorf("publish source = %v, %v; want ErrUnverifiedTLS", src, err)
	}
	_, err := exportReplayCase(ctx, dsn, sre.NewUUID(), false)
	if !errors.Is(err, databricksauth.ErrUnverifiedTLS) ||
		err.Error() != "connect to the control database: "+want {
		t.Errorf("export-replay = %v, want the refusal under its connect error", err)
	}
	ledgerErr := errOut
	code, out, errOut = runHistoryCLI(t, map[string]string{"SAGE_HISTORY_MONITORED_DSN": dsn,
		"SAGE_META_DB": dsn}, "status", "--database-id", "3")
	if code != 1 || out != "" || errOut != "history status: monitored database: "+want+"\n" {
		t.Errorf("history: exit %d stdout %q stderr %q, want 1 and the refusal", code, out,
			errOut)
	}
	if strings.Contains(ledgerErr+errOut+fmt.Sprint(err), cliPoolSecret) {
		t.Errorf("a refusal printed the password: %q %q %v", ledgerErr, errOut, err)
	}
}

// Boundary: verify-ca, the mode just below verify-full, is refused like
// every weaker one by the verifier-backtest default open and its command.
func TestCLIPool_BacktestOpenRefusesEveryUnverifiedMode(t *testing.T) {
	cliPoolLakebaseEnv(t, "projects/p/branches/b/endpoints/e", "")
	ctx := context.Background()
	for _, mode := range []string{"disable", "allow", "prefer", "require", "verify-ca"} {
		pool, err := defaultBacktestDeps().open(ctx, cliPoolLakeDSN(mode))
		if pool != nil {
			pool.Close()
		}
		if pool != nil || !errors.Is(err, databricksauth.ErrUnverifiedTLS) {
			t.Errorf("sslmode=%s: pool %v, error %v; want ErrUnverifiedTLS", mode,
				pool != nil, err)
		}
	}
	deps := defaultBacktestDeps()
	deps.load = func(string) (*config.Config, error) {
		cfg := config.DefaultConfig()
		cfg.Databases = []config.DatabaseConfig{{Name: "lake", Host: cliPoolLakeHost,
			Port: 5432, User: "sage", Password: cliPoolSecret, Database: "app",
			SSLMode: "verify-ca"}}
		return cfg, nil
	}
	var out, errOut bytes.Buffer
	code := runVerifierBacktest(ctx, []string{"--database", "lake"}, deps, &out, &errOut)
	want := "verifier-backtest lake: connect: " + databricksauth.ErrUnverifiedTLS.Error() + "\n"
	if code != 1 || out.Len() != 0 || errOut.String() != want {
		t.Fatalf("exit %d stdout %q stderr %q, want 1 and %q", code, out.String(),
			errOut.String(), want)
	}
}

// Error propagation: history names the database it could not open; with
// only the meta host mapped to Lakebase, the monitored pool opens (lazily)
// and the meta one is refused.
func TestCLIPool_HistoryNamesTheRefusedMetaDatabase(t *testing.T) {
	cliPoolLakebaseEnv(t, "", `{"`+cliPoolLakeHost+`": "projects/p/branches/b/endpoints/e"}`)
	env := map[string]string{"SAGE_HISTORY_MONITORED_DSN": "postgres://m@127.0.0.1:1/m?" +
		"sslmode=disable", "SAGE_META_DB": cliPoolLakeDSN("prefer")}
	code, out, errOut := runHistoryCLI(t, env, "migrate", "--database-id", "3")
	want := "history migrate: meta database: " + databricksauth.ErrUnverifiedTLS.Error() + "\n"
	if code != 1 || out != "" || errOut != want {
		t.Fatalf("exit %d stdout %q stderr %q, want 1 and %q", code, out, errOut, want)
	}
}

// Invalid input: a DSN that does not parse fails every command's open path
// with sagepool's fixed message (doctor keeps its own), and no output
// carries the password or a piece of a split one. pgx's parse error, which
// these paths printed as it was, kept a piece of each split password.
func TestCLIPool_BadDSNKeepsItsMessageAndNeverPrintsThePassword(t *testing.T) {
	cliPoolLakebaseEnv(t, "", "")
	for name, dsn := range map[string]string{
		"port":          cliPoolBadDSN,
		"@ in password": cliPoolSplitDSN("@", "notaport"),
		": in password": cliPoolSplitDSN(":", "notaport"),
		"/ in password": cliPoolSplitDSN("/", "5432"),
	} {
		t.Run(name, func(t *testing.T) {
			for _, p := range cliPoolBadDSNOutputs(t, dsn) {
				for _, piece := range []string{cliPoolSecret, cliPoolHalfOne, cliPoolHalfTwo} {
					if strings.Contains(p, piece) {
						t.Errorf("output carries %q of the password: %q", piece, p)
					}
				}
			}
		})
	}
}

// cliPoolBadDSNOutputs runs dsn through each command's open path, failing
// t where one does not say exactly that the DSN does not parse, and returns
// what each printed (exit code, stdout, stderr) or returned.
func cliPoolBadDSNOutputs(t *testing.T, dsn string) []string {
	t.Helper()
	ctx, msg := context.Background(), sagepool.ErrUnparseableDSN.Error()
	var printed []string
	check := func(what, got, want string) {
		t.Helper()
		if printed = append(printed, got); got != want {
			t.Errorf("%s: %q, want %q", what, got, want)
		}
	}
	isFixed := func(what string, err error) {
		t.Helper()
		check(what, fmt.Sprint(errors.Is(err, sagepool.ErrUnparseableDSN), " ", err),
			"true "+msg)
	}
	code, out, errOut := runLedgerCLI(t, map[string]string{"SAGE_META_DB": dsn})
	check("ledger", fmt.Sprint(code, " ", out, errOut), "2 ledger: "+msg+"\n"+ledgerUsage+"\n")
	code, out, errOut = runLedgerCLI(t, map[string]string{"SAGE_META_DB": dsn},
		"--database", "orders")
	check("ledger --database", fmt.Sprint(code, " ", out, errOut),
		"2 ledger: connect to orders: "+msg+"\n")
	src, err := openPublishSource(ctx, dsn)
	isFixed("publish source", err)
	check("publish source opened", fmt.Sprint(src != nil), "false")
	code, out, errOut = runExport(t, map[string]string{exportDSNEnv: dsn},
		"--investigation", string(sre.NewUUID()))
	check("export-replay", fmt.Sprint(code, " ", out, errOut),
		"1 bench export-replay: connect to the control database: "+msg+"\n")
	pool, err := defaultBacktestDeps().open(ctx, dsn)
	isFixed("verifier-backtest open", err)
	if pool != nil {
		pool.Close()
	}
	check("verifier-backtest opened", fmt.Sprint(pool != nil), "false")
	code, out, errOut = runHistoryCLI(t, map[string]string{
		"SAGE_HISTORY_MONITORED_DSN": dsn, "SAGE_META_DB": "postgres://m@h/meta"},
		"status", "--database-id", "3")
	check("history", fmt.Sprint(code, " ", out, errOut),
		"1 history status: monitored database: "+msg+"\n")
	code, out, errOut = runDoctorForTest(t, "--pg-url", dsn)
	check("doctor", fmt.Sprint(code, " ", out, errOut), "2 doctor: parse --pg-url: not a "+
		"PostgreSQL URL or key=value connection string (the string does not parse)\n")
	return printed
}
