package evidence

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "internal/evidence"))
}

func freshDB(t *testing.T, label string) *pgxpool.Pool {
	t.Helper()
	dsn := testdb.CreateDatabase(t, label)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return pool
}

func mustExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// seedAction records an executed, verified action with its decision, queue
// entry, outcome and a correlated pgaudit record, and returns its id.
func seedAction(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	ctx := context.Background()
	var decision, action int64
	err := pool.QueryRow(ctx, `INSERT INTO sage.decision (feature, intent, verdict,
		risk_tier, reason, evidence_id, policy_version) VALUES ('executor',
		'create_index', 'queue_approval', 'safe', 'missing index', 'ev-1', 3)
		RETURNING id`).Scan(&decision)
	if err != nil {
		t.Fatalf("seed decision: %v", err)
	}
	err = pool.QueryRow(ctx, `INSERT INTO sage.action_log (action_type, sql_executed,
		rollback_sql, outcome, decision_id) VALUES ('create_index',
		'CREATE INDEX CONCURRENTLY orders_status ON orders (status)',
		'DROP INDEX CONCURRENTLY orders_status', 'pending', $1) RETURNING id`,
		decision).Scan(&action)
	if err != nil {
		t.Fatalf("seed action: %v", err)
	}
	mustExec(t, pool, "UPDATE sage.action_log SET outcome = 'success' WHERE id = $1", action)
	mustExec(t, pool, `INSERT INTO sage.verification (decision_id, action_log_id,
		criterion, baseline, minimum_samples, next_evaluation_at, hard_deadline_at,
		verdict) VALUES ($1, $2, '{}', '{}', 3, now(), now(), 'success')`, decision, action)
	mustExec(t, pool, `INSERT INTO sage.guard_pgaudit_events (database_name, logged_at,
		audit_type, class, command, statement, correlated_by, action_id) VALUES
		('orders', now(), 'SESSION', 'DDL', 'CREATE INDEX', 'CREATE INDEX ...',
		'statement', $1)`, action)
	return action
}

// readPack unpacks a .tar.gz into name -> content.
func readPack(t *testing.T, raw []byte) map[string][]byte {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	tr := tar.NewReader(gz)
	out := map[string][]byte{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("tar: %v", err)
		}
		body, _ := io.ReadAll(tr)
		out[h.Name] = body
	}
}

// writePack re-packs files (to simulate tampering).
func writePack(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))})
		_, _ = tw.Write(body)
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func build(t *testing.T, pool *pgxpool.Pool, scope Scope, key ed25519.PrivateKey) []byte {
	t.Helper()
	pack, err := Build(context.Background(), pool, scope, key, time.Now())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var buf bytes.Buffer
	if err := pack.WriteTarGz(&buf); err != nil {
		t.Fatalf("write: %v", err)
	}
	return buf.Bytes()
}

// A change's pack holds the request, decision (policy version), approvals,
// verification, rollback, the database's own pgaudit record and the
// chain proof, all listed with their hashes.
func TestActionPackContents(t *testing.T) {
	pool := freshDB(t, "evidence_action")
	id := seedAction(t, pool)
	files := readPack(t, build(t, pool, Scope{Database: "orders", ActionID: id}, nil))
	for _, name := range []string{"manifest.json", "SHA256SUMS", "action.json",
		"decision.json", "queue.json", "verification.json", "outcome.json",
		"pgaudit.json", "chain.json"} {
		if _, ok := files[name]; !ok {
			t.Fatalf("pack lacks %s (has %v)", name, keys(files))
		}
	}
	if !strings.Contains(string(files["action.json"]), "DROP INDEX CONCURRENTLY") ||
		!strings.Contains(string(files["decision.json"]), `"policy_version": 3`) &&
			!strings.Contains(string(files["decision.json"]), `"policy_version":3`) {
		t.Fatalf("action/decision content: %s / %s", files["action.json"],
			files["decision.json"])
	}
	if !strings.Contains(string(files["verification.json"]), `"verdict": "success"`) ||
		!strings.Contains(string(files["pgaudit.json"]), "CREATE INDEX") {
		t.Fatalf("verification/pgaudit content missing")
	}
	assertChainProof(t, files["chain.json"])
	var m Manifest
	if err := json.Unmarshal(files["manifest.json"], &m); err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if m.Format != FormatV1 || m.Scope.ActionID != id || !m.ChainOK || len(m.Files) < 8 {
		t.Fatalf("manifest = %+v", m)
	}
	for _, f := range m.Files {
		sum := sha256.Sum256(files[f.Name])
		if hex.EncodeToString(sum[:]) != f.SHA256 || int64(len(files[f.Name])) != f.Bytes {
			t.Fatalf("manifest hash of %s does not match", f.Name)
		}
	}
	if _, err := Verify(bytes.NewReader(writePack(t, files)), nil); err != nil {
		t.Fatalf("an intact unsigned pack failed verification: %v", err)
	}
}

// SHA256SUMS lists every file, in sha256sum -c format.
func TestSHA256SUMSFormat(t *testing.T) {
	pool := freshDB(t, "evidence_sums")
	id := seedAction(t, pool)
	files := readPack(t, build(t, pool, Scope{Database: "orders", ActionID: id}, nil))
	lines := strings.Split(strings.TrimSpace(string(files["SHA256SUMS"])), "\n")
	if len(lines) != len(files)-1 {
		t.Fatalf("SHA256SUMS lists %d files, pack has %d others", len(lines), len(files)-1)
	}
	for _, line := range lines {
		sum, name, ok := strings.Cut(line, "  ")
		if !ok || len(sum) != 64 {
			t.Fatalf("bad line %q", line)
		}
		got := sha256.Sum256(files[name])
		if hex.EncodeToString(got[:]) != sum {
			t.Fatalf("%s: sum mismatch", name)
		}
	}
}

// A signed pack verifies with its key, and fails with any other key, when
// unsigned, or when a file changed even with the sums recomputed.
func TestSignedPack(t *testing.T) {
	pool := freshDB(t, "evidence_signed")
	id := seedAction(t, pool)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	raw := build(t, pool, Scope{Database: "orders", ActionID: id}, priv)
	m, err := Verify(bytes.NewReader(raw), pub)
	if err != nil || m.Signer == nil || m.Signer.KeyID != KeyID(pub) {
		t.Fatalf("signed pack: %+v, %v", m.Signer, err)
	}
	if _, err := Verify(bytes.NewReader(raw), other); !errors.Is(err, ErrSignature) {
		t.Fatalf("wrong key: err = %v", err)
	}
	unsigned := build(t, pool, Scope{Database: "orders", ActionID: id}, nil)
	if _, err := Verify(bytes.NewReader(unsigned), pub); !errors.Is(err, ErrUnsigned) {
		t.Fatalf("unsigned with a trusted key: err = %v", err)
	}
	files := readPack(t, raw)
	files["action.json"] = bytes.Replace(files["action.json"], []byte("success"),
		[]byte("failed!"), 1)
	if _, err := Verify(bytes.NewReader(writePack(t, files)), pub); !errors.Is(err,
		ErrTampered) {
		t.Fatalf("edited file: err = %v", err)
	}
	resealed := reseal(t, files)
	if _, err := Verify(bytes.NewReader(writePack(t, resealed)), pub); !errors.Is(err,
		ErrSignature) {
		t.Fatalf("edited file with recomputed sums: err = %v", err)
	}
	if _, err := Verify(bytes.NewReader(writePack(t, resealed)), nil); err != nil {
		t.Fatalf("hash-only verification of a consistent pack: %v", err)
	}
}

// reseal recomputes manifest hashes and SHA256SUMS after an edit, as an
// attacker without the signing key would.
func reseal(t *testing.T, files map[string][]byte) map[string][]byte {
	t.Helper()
	var m Manifest
	if err := json.Unmarshal(files["manifest.json"], &m); err != nil {
		t.Fatalf("manifest: %v", err)
	}
	for i, f := range m.Files {
		sum := sha256.Sum256(files[f.Name])
		m.Files[i].SHA256, m.Files[i].Bytes = hex.EncodeToString(sum[:]),
			int64(len(files[f.Name]))
	}
	files["manifest.json"], _ = json.MarshalIndent(m, "", "  ")
	var sums strings.Builder
	for name, body := range files {
		if name == "SHA256SUMS" {
			continue
		}
		sum := sha256.Sum256(body)
		sums.WriteString(hex.EncodeToString(sum[:]) + "  " + name + "\n")
	}
	files["SHA256SUMS"] = []byte(sums.String())
	return files
}

// A window pack holds the audit of the window only, with each chain's
// verification; a broken chain is reported in the pack, not hidden.
func TestWindowPack(t *testing.T) {
	pool := freshDB(t, "evidence_window")
	seedAction(t, pool)
	mustExec(t, pool, `INSERT INTO sage.action_log (action_type, sql_executed,
		executed_at) VALUES ('vacuum', 'VACUUM old', now() - interval '10 days')`)
	mustExec(t, pool, `INSERT INTO sage.auth_audit (event, detail)
		VALUES ('break_glass_login', '{}')`)
	mustExec(t, pool, `INSERT INTO sage.config_audit (key, old_value, new_value)
		VALUES ('trust.level', 'observation', 'advisory')`)
	scope := Scope{Database: "orders", From: time.Now().Add(-time.Hour),
		To: time.Now().Add(time.Hour)}
	files := readPack(t, build(t, pool, scope, nil))
	actions := string(files["actions.json"])
	if !strings.Contains(actions, "orders_status") || strings.Contains(actions, "VACUUM old") {
		t.Fatalf("actions.json window wrong: %s", actions)
	}
	if !strings.Contains(string(files["auth_audit.json"]), "break_glass_login") ||
		!strings.Contains(string(files["config_audit.json"]), "advisory") {
		t.Fatalf("auth/config audit missing")
	}
	var m Manifest
	_ = json.Unmarshal(files["manifest.json"], &m)
	if !m.ChainOK {
		t.Fatalf("intact chains reported broken: %s", files["chains.json"])
	}
	mustExec(t, pool, `UPDATE sage.action_log SET sql_executed = 'SELECT 1'
		WHERE sql_executed LIKE 'CREATE INDEX%'`)
	files = readPack(t, build(t, pool, scope, nil))
	_ = json.Unmarshal(files["manifest.json"], &m)
	if m.ChainOK || !strings.Contains(string(files["chains.json"]), "row_edited") {
		t.Fatalf("a tampered chain was not reported: %s", files["chains.json"])
	}
}

// Invalid scopes and unknown actions are refused distinguishably; an empty
// window still builds.
func TestScopeErrors(t *testing.T) {
	pool := freshDB(t, "evidence_scope")
	ctx := context.Background()
	now := time.Now()
	bad := []Scope{{}, {ActionID: -1}, {From: now, To: now.Add(-time.Hour)},
		{From: now.Add(-40 * 24 * time.Hour), To: now},
		{ActionID: 1, From: now.Add(-time.Hour), To: now}}
	for i, s := range bad {
		if _, err := Build(ctx, pool, s, nil, now); !errors.Is(err, ErrScope) {
			t.Fatalf("scope %d: err = %v, want ErrScope", i, err)
		}
	}
	if _, err := Build(ctx, pool, Scope{ActionID: 999999}, nil, now); !errors.Is(err,
		ErrNotFound) {
		t.Fatalf("unknown action: err = %v", err)
	}
	empty := readPack(t, build(t, pool, Scope{From: now.Add(-time.Minute), To: now}, nil))
	if strings.TrimSpace(string(empty["actions.json"])) != "[]" {
		t.Fatalf("empty window actions = %s", empty["actions.json"])
	}
	if _, err := Build(ctx, nil, Scope{ActionID: 1}, nil, now); err == nil {
		t.Fatalf("nil database accepted")
	}
}

// Signing keys load from PKCS#8 PEM; anything else is refused.
func TestLoadSigningKey(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	pemText, err := MarshalSigningKey(priv)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got, err := LoadSigningKey(pemText)
	if err != nil || !got.Equal(priv) {
		t.Fatalf("round trip: %v", err)
	}
	for _, bad := range []string{"", "garbage", "-----BEGIN PRIVATE KEY-----\nAAAA\n" +
		"-----END PRIVATE KEY-----\n"} {
		if _, err := LoadSigningKey(bad); err == nil {
			t.Fatalf("bad key %q accepted", bad)
		}
	}
}

func keys(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// assertChainProof checks an action pack's chain.json: two links, verified
// clean, with the chain head.
func assertChainProof(t *testing.T, raw []byte) {
	t.Helper()
	var chain struct {
		Links  []map[string]any `json:"links"`
		Report struct {
			Problems []any `json:"problems"`
			Links    int   `json:"links"`
		} `json:"verification"`
		Head struct {
			Seq  int64  `json:"seq"`
			Hash string `json:"hash"`
		} `json:"head"`
	}
	if err := json.Unmarshal(raw, &chain); err != nil {
		t.Fatalf("chain.json: %v", err)
	}
	if len(chain.Links) != 2 || len(chain.Report.Problems) != 0 || chain.Head.Hash == "" {
		t.Fatalf("chain proof = %+v", chain)
	}
}
