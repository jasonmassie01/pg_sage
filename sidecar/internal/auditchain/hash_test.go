package auditchain

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

// The Go link hash is the documented preimage, so an auditor can recompute
// it with sha256sum: prev|seq|op|row_id|v|sealed_hash|state.
func TestLinkHashPreimage(t *testing.T) {
	l := Link{Seq: 7, RowID: 42, Op: "U", V: 1, SealedHash: "ab",
		State: `["success", null]`, PrevHash: strings.Repeat("0", 64)}
	sum := sha256.Sum256([]byte(strings.Repeat("0", 64) + `|7|U|42|1|ab|["success", null]`))
	if got, want := LinkHash(l), hex.EncodeToString(sum[:]); got != want {
		t.Fatalf("LinkHash = %s, want %s", got, want)
	}
}

// Every field is bound: changing any one changes the hash.
func TestLinkHashBindsEveryField(t *testing.T) {
	base := Link{Seq: 1, RowID: 2, Op: "I", V: 1, SealedHash: "s", State: "x", PrevHash: "p"}
	mutants := []Link{base, base, base, base, base, base, base}
	mutants[0].Seq = 2
	mutants[1].RowID = 3
	mutants[2].Op = "D"
	mutants[3].V = 2
	mutants[4].SealedHash = "t"
	mutants[5].State = "y"
	mutants[6].PrevHash = "q"
	seen := map[string]bool{LinkHash(base): true}
	for i, m := range mutants {
		h := LinkHash(m)
		if seen[h] {
			t.Fatalf("mutant %d collides with an earlier hash", i)
		}
		seen[h] = true
	}
}

// The built-in specs are well formed: a chain name, a sage table, a
// version, and sealed columns that include the row id.
func TestBuiltinSpecs(t *testing.T) {
	names := map[string]bool{}
	for _, s := range Specs() {
		if err := s.validate(); err != nil {
			t.Fatalf("spec %q: %v", s.Chain, err)
		}
		if names[s.Chain] {
			t.Fatalf("duplicate chain %q", s.Chain)
		}
		names[s.Chain] = true
		if !strings.HasPrefix(s.Table, "sage.") || len(s.Sealed) == 0 ||
			!strings.Contains(s.Sealed[0], "id") {
			t.Fatalf("spec %+v: sealed columns must start with the row id", s)
		}
	}
	for _, want := range []string{"action_log", "auth_audit", "config_audit"} {
		if !names[want] {
			t.Fatalf("built-in chain %q missing", want)
		}
	}
	if len(ActionLog.State) == 0 || len(AuthAudit.State) != 0 {
		t.Fatalf("action_log tracks state; auth_audit is append-only")
	}
}

// Invalid specs are refused before any SQL is built.
func TestSpecValidation(t *testing.T) {
	bad := []Spec{
		{},
		{Chain: "Bad Name", Table: "sage.x", Version: 1, Sealed: []string{"%[1]s.id"}},
		{Chain: "x", Table: "public.x", Version: 1, Sealed: []string{"%[1]s.id"}},
		{Chain: "x", Table: "sage.x", Version: 0, Sealed: []string{"%[1]s.id"}},
		{Chain: "x", Table: "sage.x", Version: 1},
		{Chain: "x", Table: "sage.x; DROP", Version: 1, Sealed: []string{"%[1]s.id"}},
	}
	for i, s := range bad {
		if err := s.validate(); err == nil {
			t.Fatalf("bad spec %d accepted: %+v", i, s)
		}
	}
}

// The install SQL is idempotent by construction and every statement pg_sage
// runs from it is tagged.
func TestInstallSQLShape(t *testing.T) {
	sql := InstallSQL(ActionLog)
	for _, want := range []string{
		"CREATE CONSTRAINT TRIGGER audit_chain_v1_id",
		"DEFERRABLE INITIALLY DEFERRED",
		"BEFORE TRUNCATE",
		"IF NOT EXISTS",
		"/* pg_sage audit_chain v1 */",
		"pg_advisory_xact_lock",
	} {
		if !strings.Contains(sql, want) {
			t.Fatalf("install SQL lacks %q", want)
		}
	}
	if strings.Contains(InstallSQL(AuthAudit), "audit_chain_v1_u") {
		t.Fatalf("an append-only chain must not install an update trigger")
	}
	if !strings.Contains(MigrationSQL(), "CREATE TABLE IF NOT EXISTS sage.audit_chain_link") {
		t.Fatalf("migration does not create the link table")
	}
}

// The guard_query_audit chain installs only once its table exists, so the
// migration is safe before the core workstream creates it.
func TestOptionalChainIsConditional(t *testing.T) {
	sql := InstallSQL(GuardQueryAudit)
	if !strings.Contains(sql, "to_regclass('sage.guard_query_audit')") {
		t.Fatalf("guard_query_audit install is not conditional on the table")
	}
}
