package main

import (
	"bytes"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CHECK-03 (cli-01a): the token never appears in stdout, stderr or a log
// line, on success, on every refusal, with --json, and even when the
// sidecar echoes it back in an error.
func TestCLI_TokenNeverPrintedOrLogged(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{
		Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	f, srv := tokenEchoSidecar(t)
	closed := httptest.NewServer(newReplay())
	closedURL := closed.URL
	closed.Close()

	var printed strings.Builder
	for _, run := range []struct {
		url  string
		args []string
	}{
		{srv.URL, []string{"plan"}}, {srv.URL, []string{"plan", "--json"}},
		{srv.URL, []string{"approve", "42", "--yes"}}, {srv.URL, []string{"apply", "43"}},
		{srv.URL, []string{"explain", "--database", "orders", "--json"}},
		{closedURL, []string{"approve", "42", "--yes"}},
		{srv.URL, []string{"plan", "--bogus"}}, {"", []string{"plan"}},
	} {
		_, out, errOut := runCLIAt(t, run.url, "", run.args...)
		printed.WriteString(out + errOut)
	}
	if !strings.Contains(printed.String(), "[redacted]") {
		t.Fatalf("the echoed token was not redacted (or not printed at all):\n%s",
			printed.String())
	}
	for name, text := range map[string]string{"output": printed.String(),
		"logs": logs.String()} {
		if strings.Contains(text, cliTestToken) || strings.Contains(text, cliTestToken[8:24]) {
			t.Fatalf("the token appears in the %s:\n%s", name, text)
		}
	}
	for _, r := range f.got {
		if strings.Contains(r.query, cliTestToken) {
			t.Fatalf("the token was sent in a query string: %s", r.query)
		}
	}
}

// tokenEchoSidecar answers every decision-loop read, and echoes the token
// back in two refusals.
func tokenEchoSidecar(t *testing.T) (*replaySidecar, *httptest.Server) {
	t.Helper()
	f := newReplay()
	f.on(t, "GET /api/v1/inbox", 200, "inbox_two.json")
	f.on(t, "GET /api/v1/plans", 200, "plans.json")
	f.on(t, "GET /api/v1/actions/pending", 200, "actions_pending.json")
	f.on(t, "GET /api/v1/whynot", 200, "whynot_901.json")
	f.on(t, "GET /api/v1/approvals/42", 200, "card_42.json")
	f.on(t, "POST /api/v1/approvals/42/approve", 500,
		`{"error":"token `+cliTestToken+` was refused upstream"}`)
	f.on(t, "GET /api/v1/approvals/43", 401, `{"error":"bad token `+cliTestToken+`"}`)
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

// CHECK-04 (cli-01a): docs/cli.md lists every subcommand with its exit
// codes, and its usage blocks are what --help prints.
func TestCLI_DocsMatchHelp(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "cli.md"))
	if err != nil {
		t.Fatalf("docs/cli.md: %v", err)
	}
	doc := strings.ReplaceAll(string(raw), "\r\n", "\n")
	for _, cmd := range []string{"plan", "approve", "apply", "explain"} {
		code, out, errOut := runCLIAt(t, "", "", cmd, "--help")
		help := strings.TrimSpace(out)
		if code != 0 || help == "" || errOut != "" {
			t.Fatalf("%s --help: exit %d, out %q, err %q", cmd, code, out, errOut)
		}
		if !strings.Contains(doc, "```text\n"+help+"\n```") {
			t.Errorf("docs/cli.md has no usage block equal to `%s --help`:\n%s", cmd, help)
		}
	}
	if !strings.Contains(doc, "```text\n"+ledgerUsage+"\n```") {
		t.Errorf("docs/cli.md has no usage block equal to the ledger usage")
	}
	for _, heading := range []string{"## plan", "## approve", "## apply", "## explain",
		"## ledger", "## doctor"} {
		at := strings.Index(doc, "\n"+heading+"\n")
		if at < 0 {
			t.Errorf("docs/cli.md has no %q section", heading)
			continue
		}
		section := doc[at+1:]
		if next := strings.Index(section[1:], "\n## "); next >= 0 {
			section = section[:next+1]
		}
		if !strings.Contains(section, "Exit codes") {
			t.Errorf("docs/cli.md %q lists no exit codes", heading)
		}
	}
	if !strings.Contains(doc, "| 64 |") {
		t.Errorf("docs/cli.md does not document the status form's usage exit 64")
	}
}
