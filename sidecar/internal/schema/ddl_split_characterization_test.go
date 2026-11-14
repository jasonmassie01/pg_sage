package schema

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/auditchain"
)

// config-31: the DDL constants moved out of bootstrap.go byte for byte.
// The migration ledger pins the step SQL; this test also pins the base
// tables and fullSchemaDDL, which a fresh install runs and the ledger
// never sees. testdata/ddl_constants.sha256 was generated on the unsplit
// bootstrap.go.
// No concurrent access tests: the constants are immutable package state.

const ddlHashesFile = "testdata/ddl_constants.sha256"

// baseTableConstants are the base-table constants bootstrap.go held.
func baseTableConstants() map[string]string {
	return map[string]string{
		"fullSchemaDDL":       fullSchemaDDL,
		"ddlActionLog":        ddlActionLog,
		"ddlSnapshots":        ddlSnapshots,
		"ddlFindings":         ddlFindings,
		"ddlExplainCache":     ddlExplainCache,
		"ddlBriefings":        ddlBriefings,
		"ddlCryptoMeta":       ddlCryptoMeta,
		"ddlConfig":           ddlConfig,
		"ddlAlertLog":         ddlAlertLog,
		"ddlQueryHints":       ddlQueryHints,
		"ddlExplainSourceIdx": ddlExplainSourceIdx,
		"ddlUsers":            ddlUsers,
		"ddlSessions":         ddlSessions,
		"ddlActionQueue":      ddlActionQueue,
		"ddlIncidents":        ddlIncidents,
		"ddlSchemaFindings":   ddlSchemaFindings,
		"ddlExplainResults":   ddlExplainResults,
		"ddlHealthHistory":    ddlHealthHistory,
		"ddlQueryStore":       ddlQueryStore,
	}
}

// currentDDLHashes hashes every base-table constant, every expectedTables
// entry, every migration step and the audit chain's DDL, keyed by kind.
func currentDDLHashes() map[string]string {
	out := map[string]string{}
	for name, sql := range baseTableConstants() {
		out["base."+name] = ddlSHA256(sql)
	}
	for _, tbl := range expectedTables {
		out["table."+tbl.name] = ddlSHA256(tbl.ddl)
	}
	for _, s := range migrationSteps() {
		out["step."+s.name] = ddlSHA256(s.sql)
	}
	out["auditchain.MigrationSQL"] = ddlSHA256(auditchain.MigrationSQL())
	return out
}

func ddlSHA256(sql string) string {
	sum := sha256.Sum256([]byte(sql))
	return hex.EncodeToString(sum[:])
}

// parseDDLHashes reads "key sha256" lines; '#' starts a comment line.
func parseDDLHashes(r io.Reader) (map[string]string, error) {
	out := map[string]string{}
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, sum, ok := strings.Cut(line, " ")
		if !ok || key == "" || len(sum) != 64 {
			return nil, fmt.Errorf("line %d %q is not 'key sha256'", n, line)
		}
		if _, err := hex.DecodeString(sum); err != nil {
			return nil, fmt.Errorf("line %d: checksum %q is not hex: %w", n, sum, err)
		}
		if _, dup := out[key]; dup {
			return nil, fmt.Errorf("line %d: key %s listed twice", n, key)
		}
		out[key] = sum
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading DDL hashes: %w", err)
	}
	return out, nil
}

// diffDDLHashes reports every pinned key that is gone or changed, and every
// base or table key that is not pinned. A new migration step is allowed:
// testdata/migration_checksums.txt pins it.
func diffDDLHashes(golden, current map[string]string) []string {
	var problems []string
	for key, want := range golden {
		got, ok := current[key]
		switch {
		case !ok:
			problems = append(problems, key+": pinned but no longer defined")
		case got != want:
			problems = append(problems, fmt.Sprintf(
				"%s: changed (pinned %s, now %s)", key, want, got))
		}
	}
	for key, got := range current {
		if _, ok := golden[key]; ok || strings.HasPrefix(key, "step.") {
			continue
		}
		problems = append(problems, fmt.Sprintf("%s: not pinned, add '%s %s'",
			key, key, got))
	}
	sort.Strings(problems)
	return problems
}

func readGoldenDDLHashes(t *testing.T) map[string]string {
	t.Helper()
	f, err := os.Open(ddlHashesFile)
	if err != nil {
		t.Fatalf("open %s: %v", ddlHashesFile, err)
	}
	defer f.Close()
	golden, err := parseDDLHashes(f)
	if err != nil {
		t.Fatalf("parse %s: %v", ddlHashesFile, err)
	}
	return golden
}

func TestDDLConstants_MatchTheGoldenHashes(t *testing.T) {
	golden := readGoldenDDLHashes(t)
	for _, p := range diffDDLHashes(golden, currentDDLHashes()) {
		t.Error(p)
	}
}

// Boundary: the golden file pins all 19 base constants, every table and the
// first and last migration steps the split had to keep in place.
func TestDDLConstants_GoldenCoversEveryBaseTable(t *testing.T) {
	golden := readGoldenDDLHashes(t)
	base := baseTableConstants()
	if len(base) != 19 {
		t.Fatalf("baseTableConstants has %d entries, want 19", len(base))
	}
	for name, sql := range base {
		if strings.TrimSpace(sql) == "" {
			t.Errorf("%s is empty", name)
		}
		if _, ok := golden["base."+name]; !ok {
			t.Errorf("base.%s is not pinned in %s", name, ddlHashesFile)
		}
	}
	for _, tbl := range expectedTables {
		if _, ok := golden["table."+tbl.name]; !ok {
			t.Errorf("table.%s is not pinned in %s", tbl.name, ddlHashesFile)
		}
	}
	for _, key := range []string{"step.ddlActionLogApprovalCols",
		"step.ddlSchemaFindingsLintRunner", "step.ddlGameDayExercised",
		"auditchain.MigrationSQL"} {
		if _, ok := golden[key]; !ok {
			t.Errorf("%s is not pinned in %s", key, ddlHashesFile)
		}
	}
}

// Invalid input: one changed byte, a dropped constant and an unpinned table
// must each be reported, so a broken comparison cannot pass vacuously.
func TestDiffDDLHashes_ReportsEveryDrift(t *testing.T) {
	golden := map[string]string{
		"base.ddlConfig":     ddlSHA256(ddlConfig),
		"base.ddlUsers":      ddlSHA256(ddlUsers),
		"step.ddlUsersOAuth": ddlSHA256(ddlUsersOAuth),
	}
	current := map[string]string{
		"base.ddlConfig":       ddlSHA256(ddlConfig + " "),
		"step.ddlUsersOAuth":   ddlSHA256(ddlUsersOAuth),
		"table.health_history": ddlSHA256(ddlHealthHistory),
		"step.ddlNewStep":      ddlSHA256("SELECT 1"),
	}
	got := diffDDLHashes(golden, current)
	want := []string{
		"base.ddlConfig: changed",
		"base.ddlUsers: pinned but no longer defined",
		"table.health_history: not pinned",
	}
	if len(got) != len(want) {
		t.Fatalf("diffDDLHashes = %q, want %d problems like %q", got, len(want), want)
	}
	for i := range want {
		if !strings.HasPrefix(got[i], want[i]) {
			t.Errorf("problem %d = %q, want prefix %q", i, got[i], want[i])
		}
	}
}

// Nil/empty: identical maps report nothing; an empty current reports every
// pinned key; a nil golden reports every base and table key but no step.
func TestDiffDDLHashes_EmptyAndNil(t *testing.T) {
	cur := currentDDLHashes()
	if p := diffDDLHashes(cur, cur); len(p) != 0 {
		t.Fatalf("identical maps reported %q", p)
	}
	golden := map[string]string{"base.ddlConfig": ddlSHA256(ddlConfig)}
	p := diffDDLHashes(golden, nil)
	if len(p) != 1 || !strings.HasPrefix(p[0], "base.ddlConfig: pinned but") {
		t.Fatalf("empty current = %q, want base.ddlConfig gone", p)
	}
	p = diffDDLHashes(nil, cur)
	wantN := len(baseTableConstants()) + len(expectedTables) + 1
	if len(p) != wantN {
		t.Fatalf("nil golden reported %d problems, want %d (base, table, auditchain)",
			len(p), wantN)
	}
	for _, line := range p {
		if strings.HasPrefix(line, "step.") {
			t.Errorf("a new step must not be reported: %q", line)
		}
	}
}

func TestParseDDLHashes_RejectsMalformedLines(t *testing.T) {
	sum := ddlSHA256("x")
	cases := map[string]string{
		"no checksum":    "base.ddlConfig\n",
		"short checksum": "base.ddlConfig abc\n",
		"not hex":        "base.ddlConfig " + strings.Repeat("z", 64) + "\n",
		"duplicate key":  "base.ddlConfig " + sum + "\nbase.ddlConfig " + sum + "\n",
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := parseDDLHashes(strings.NewReader(in))
			if err == nil {
				t.Fatalf("parseDDLHashes(%q) = %v, want an error", in, got)
			}
		})
	}
	got, err := parseDDLHashes(strings.NewReader("# c\r\n\r\nbase.ddlConfig " + sum + "\r\n"))
	if err != nil || len(got) != 1 || got["base.ddlConfig"] != sum {
		t.Fatalf("CRLF file with a comment = %v, %v; want one entry", got, err)
	}
}
