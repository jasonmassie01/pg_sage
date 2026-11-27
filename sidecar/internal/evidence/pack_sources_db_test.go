package evidence

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// audit-07: in fleet mode sign-ins and config changes are written to the
// control database, but a window pack read them from the audited one, so
// its auth_audit.json and config_audit.json were empty or another
// database's, with no note and chain_ok true.

// No concurrency tests: a builder is per request and holds no shared state.

// buildFrom builds a pack from src and returns its manifest and files,
// after checking the archive verifies.
func buildFrom(t *testing.T, src Sources, scope Scope) (Manifest, map[string][]byte) {
	t.Helper()
	pack, err := Build(context.Background(), src, scope, nil, time.Now())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var buf bytes.Buffer
	if err := pack.WriteTarGz(&buf); err != nil {
		t.Fatalf("write: %v", err)
	}
	m, err := Verify(bytes.NewReader(buf.Bytes()), nil)
	if err != nil {
		t.Fatalf("the pack failed verification: %v", err)
	}
	return m, readPack(t, buf.Bytes())
}

// entry is the manifest entry of file name.
func entry(t *testing.T, m Manifest, name string) FileEntry {
	t.Helper()
	for _, f := range m.Files {
		if f.Name == name {
			return f
		}
	}
	t.Fatalf("manifest lists no %s: %+v", name, m.Files)
	return FileEntry{}
}

// proofOf is the chains.json proof of chain.
type windowProof struct {
	Chain        string `json:"chain"`
	Source       string `json:"source"`
	Note         string `json:"note"`
	Verification *struct {
		Links    int   `json:"links"`
		Problems []any `json:"problems"`
	} `json:"verification"`
}

func proofOf(t *testing.T, files map[string][]byte, chain string) windowProof {
	t.Helper()
	var proofs []windowProof
	if err := json.Unmarshal(files["chains.json"], &proofs); err != nil {
		t.Fatalf("chains.json: %v", err)
	}
	for _, p := range proofs {
		if p.Chain == chain {
			return p
		}
	}
	t.Fatalf("chains.json has no %s proof: %s", chain, files["chains.json"])
	return windowProof{}
}

func lastHour() Scope {
	return Scope{Database: "orders", From: time.Now().Add(-time.Hour),
		To: time.Now().Add(time.Hour)}
}

// seedControlAudit writes, to the control database, a sign-in and config
// changes of database 7 (the pack's), of database 8 and a global one.
func seedControlAudit(t *testing.T, control *pgxpool.Pool) {
	t.Helper()
	mustExec(t, control, `INSERT INTO sage.auth_audit (event, detail)
		VALUES ('control_login_success', '{}')`)
	mustExec(t, control, `INSERT INTO sage.config_audit (key, old_value, new_value,
		database_id) VALUES ('trust.level', 'observation', 'advisory-for-x', 7),
		('trust.level', 'observation', 'advisory-for-y', 8),
		('llm.enabled', 'false', 'global-true', NULL)`)
}

// Two pools: the sign-ins and config changes of database 7 come from the
// control database, labelled source "control", with their chains proved
// there; database 8's change is not in the pack.
func TestWindowPackReadsControlAudit(t *testing.T) {
	monitored := freshDB(t, "evidence_src_monitored")
	control := freshDB(t, "evidence_src_control")
	seedAction(t, monitored)
	mustExec(t, monitored, `INSERT INTO sage.auth_audit (event, detail)
		VALUES ('monitored_decoy_login', '{}')`)
	seedControlAudit(t, control)
	m, files := buildFrom(t, Sources{DB: monitored, Control: control, DatabaseID: 7},
		lastHour())
	auth, config := string(files["auth_audit.json"]), string(files["config_audit.json"])
	if !strings.Contains(auth, "control_login_success") ||
		strings.Contains(auth, "monitored_decoy_login") {
		t.Fatalf("auth_audit.json = %s, want the control database's sign-in only", auth)
	}
	if !strings.Contains(config, "advisory-for-x") || !strings.Contains(config,
		"global-true") || strings.Contains(config, "advisory-for-y") {
		t.Fatalf("config_audit.json = %s, want database 7's and global changes only",
			config)
	}
	for name, want := range map[string]string{"auth_audit.json": "control",
		"config_audit.json": "control", "actions.json": "database",
		"pgaudit.json": "database", "decisions.json": "database"} {
		if got := entry(t, m, name).Source; got != want {
			t.Errorf("%s source = %q, want %q", name, got, want)
		}
	}
	if e := entry(t, m, "config_audit.json"); e.Rows == nil || *e.Rows != 2 || e.Note != "" {
		t.Fatalf("config_audit.json entry = %+v, want 2 rows and no note", e)
	}
	for _, chain := range []string{"auth_audit", "config_audit"} {
		p := proofOf(t, files, chain)
		if p.Source != "control" || p.Verification == nil || p.Verification.Links < 1 ||
			len(p.Verification.Problems) != 0 {
			t.Fatalf("%s proof = %+v, want a clean control-side verification", chain, p)
		}
	}
	// The row's chain_seq is its newest link (the outcome update): one link.
	if p := proofOf(t, files, "action_log"); p.Source != "database" ||
		p.Verification == nil || p.Verification.Links != 1 {
		t.Fatalf("action_log proof = %+v, want the audited database's newest link", p)
	}
	if !m.ChainOK || m.Format != Format || Format != "pg_sage-evidence/2" {
		t.Fatalf("manifest chain_ok %v format %q", m.ChainOK, m.Format)
	}
}

// An edited control-side config row breaks the pack's chain_ok: the proof
// is of the rows the pack holds, not of the audited database's empty table.
func TestWindowPackReportsBrokenControlChain(t *testing.T) {
	monitored := freshDB(t, "evidence_src_broken_m")
	control := freshDB(t, "evidence_src_broken_c")
	seedControlAudit(t, control)
	mustExec(t, control, `UPDATE sage.config_audit SET new_value = 'forged'
		WHERE database_id = 7`)
	m, files := buildFrom(t, Sources{DB: monitored, Control: control, DatabaseID: 7},
		lastHour())
	p := proofOf(t, files, "config_audit")
	if m.ChainOK || p.Verification == nil || len(p.Verification.Problems) == 0 {
		t.Fatalf("a forged control config row was not reported: chain_ok %v proof %+v",
			m.ChainOK, p)
	}
}

// With no id for the database the pack takes only global config changes,
// and says so in the file's note.
func TestWindowPackUnknownDatabaseTakesGlobalConfig(t *testing.T) {
	monitored := freshDB(t, "evidence_src_noid_m")
	control := freshDB(t, "evidence_src_noid_c")
	seedControlAudit(t, control)
	m, files := buildFrom(t, Sources{DB: monitored, Control: control}, lastHour())
	config := string(files["config_audit.json"])
	if !strings.Contains(config, "global-true") || strings.Contains(config, "advisory-for") {
		t.Fatalf("config_audit.json = %s, want the global change only", config)
	}
	e := entry(t, m, "config_audit.json")
	if e.Rows == nil || *e.Rows != 1 || !strings.Contains(e.Note, "global") {
		t.Fatalf("config_audit.json entry = %+v, want 1 row and a note on global keys", e)
	}
}

// goldenEntry is the part of a manifest entry that does not depend on the
// rows' content.
type goldenEntry struct {
	Name, Source string
	Rows         int
	Truncated    bool
}

// Control nil (standalone, or the control pack) reproduces today's file
// set and reads every file from the one database, config unfiltered.
func TestWindowPackWithoutControlKeepsTodaysFiles(t *testing.T) {
	pool := freshDB(t, "evidence_src_nil")
	seedAction(t, pool)
	seedControlAudit(t, pool)
	m, files := buildFrom(t, Sources{DB: pool}, lastHour())
	var got []goldenEntry
	for _, f := range m.Files {
		g := goldenEntry{Name: f.Name, Source: f.Source, Truncated: f.Truncated}
		if f.Rows != nil {
			g.Rows = *f.Rows
		}
		got = append(got, g)
	}
	want := []goldenEntry{{"actions.json", "database", 1, false},
		{"auth_audit.json", "database", 1, false}, {"chains.json", "", 0, false},
		{"config_audit.json", "database", 3, false},
		{"decisions.json", "database", 1, false},
		{"guard_query_audit.json", "database", 0, false},
		{"pgaudit.json", "database", 1, false},
		// audit-11 adds the window's policy approvals.
		{"policy_approvals.json", "database", 0, false}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("manifest files =\n%+v\nwant\n%+v", got, want)
	}
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "SHA256SUMS,actions.json,auth_audit.json,chains.json,"+
		"config_audit.json,decisions.json,guard_query_audit.json,manifest.json,pgaudit.json,"+
		"policy_approvals.json" {
		t.Fatalf("pack files = %v", names)
	}
	if m.Truncated || !m.ChainOK || entry(t, m, "actions.json").LastID == nil {
		t.Fatalf("manifest truncated %v chain_ok %v", m.Truncated, m.ChainOK)
	}
}

// A closed control pool fails the pack with an error naming the control
// database; it never yields an empty file.
func TestPackFailsOnClosedControlPool(t *testing.T) {
	monitored := freshDB(t, "evidence_src_closed_m")
	control := freshDB(t, "evidence_src_closed_c")
	action := seedAction(t, monitored)
	control.Close()
	ctx := context.Background()
	src := Sources{DB: monitored, Control: control, DatabaseID: 7}
	for _, scope := range []Scope{lastHour(), {Database: "orders", ActionID: action}} {
		pack, err := Build(ctx, src, scope, nil, time.Now())
		if err == nil || pack != nil || !strings.Contains(err.Error(), "control") {
			t.Fatalf("scope %+v: pack %v err %v, want an error naming control", scope,
				pack != nil, err)
		}
	}
}
