package recommendation

import (
	"strings"
	"testing"
)

func baseProposal() Proposal {
	return Proposal{
		DatabaseName: "prod", Category: "missing_index", Target: "public.orders",
		ObjectType: "table", Title: "t", Severity: "warning", ActionRisk: "safe",
		ForwardSQL: "CREATE INDEX CONCURRENTLY idx_a ON public.orders (a)",
		InverseSQL: "DROP INDEX CONCURRENTLY public.idx_a",
		Evidence:   map[string]any{"seq_scans": 10},
	}
}

func TestContentHashCoversActionContentOnly(t *testing.T) {
	base := baseProposal()
	h := ContentHash(base)
	if len(h) != 64 || strings.Trim(h, "0123456789abcdef") != "" {
		t.Fatalf("ContentHash = %q, want 64 lowercase hex chars", h)
	}
	same := base
	same.Evidence = map[string]any{"seq_scans": 99999}
	same.Title, same.Severity, same.Recommendation = "other", "critical", "text"
	v := int64(7)
	same.PolicyVersion = &v
	if ContentHash(same) != h {
		t.Fatal("volatile evidence, wording or policy version changed the content hash; " +
			"every cycle would invalidate approvals")
	}
	for name, mutate := range map[string]func(*Proposal){
		"forward": func(p *Proposal) { p.ForwardSQL += " WHERE a > 0" },
		"inverse": func(p *Proposal) { p.InverseSQL = "DROP INDEX public.idx_b" },
		"target":  func(p *Proposal) { p.Target = "public.customers" },
		"no inverse": func(p *Proposal) {
			p.InverseSQL = ""
		},
	} {
		changed := base
		mutate(&changed)
		if ContentHash(changed) == h {
			t.Errorf("%s change kept the same content hash", name)
		}
	}
}

func TestContentHashSeparatesFields(t *testing.T) {
	a, b := baseProposal(), baseProposal()
	a.ForwardSQL, a.InverseSQL = "SELECT 1", "SELECT 2"
	b.ForwardSQL, b.InverseSQL = "SELECT 1SELECT 2", ""
	if ContentHash(a) == ContentHash(b) {
		t.Fatal("field boundaries are ambiguous in the content hash")
	}
}

func TestIdentityKeyIncludesDatabaseCategoryTargetActionAndIndex(t *testing.T) {
	base := baseProposal()
	key := IdentityKey(base)
	if len(key) != 64 {
		t.Fatalf("IdentityKey = %q, want sha256 hex", key)
	}
	if IdentityKey(base) != key {
		t.Fatal("IdentityKey is not deterministic")
	}
	sameIdentity := base
	sameIdentity.InverseSQL = ""
	sameIdentity.Evidence = nil
	if IdentityKey(sameIdentity) != key {
		t.Fatal("inverse SQL or evidence changed the identity")
	}
	for name, mutate := range map[string]func(*Proposal){
		"database": func(p *Proposal) { p.DatabaseName = "staging" },
		"category": func(p *Proposal) { p.Category = "duplicate_index" },
		"target":   func(p *Proposal) { p.Target = "public.items" },
		"action":   func(p *Proposal) { p.ForwardSQL = "VACUUM public.orders" },
		"index": func(p *Proposal) {
			p.ForwardSQL = "CREATE INDEX CONCURRENTLY idx_a ON public.orders (b)"
		},
	} {
		changed := base
		mutate(&changed)
		if IdentityKey(changed) == key {
			t.Errorf("%s change kept the same identity", name)
		}
	}
}

// C05: two candidate indexes on one table are two recommendations; the
// same definition under another name is the same recommendation.
func TestIndexFingerprintC05(t *testing.T) {
	a := IndexFingerprint("CREATE INDEX CONCURRENTLY idx_a ON public.orders (customer_id)")
	b := IndexFingerprint("CREATE INDEX CONCURRENTLY idx_b ON public.orders (created_at)")
	renamed := IndexFingerprint(
		"create index   concurrently if not exists idx_zz on public.orders (customer_id)")
	if a == "" || b == "" {
		t.Fatalf("CREATE INDEX fingerprints are empty: %q %q", a, b)
	}
	if a == b {
		t.Fatal("different index definitions share a fingerprint (C05 collapse)")
	}
	if a != renamed {
		t.Fatalf("same definition under another name/spacing differs: %q vs %q", a, renamed)
	}
	partial := IndexFingerprint(
		"CREATE INDEX CONCURRENTLY idx_a ON public.orders (customer_id) WHERE active")
	if partial == a {
		t.Fatal("a partial index shares the full index fingerprint")
	}
	dropA := IndexFingerprint("DROP INDEX CONCURRENTLY public.idx_a")
	dropB := IndexFingerprint("DROP INDEX CONCURRENTLY public.idx_b")
	if dropA == "" || dropA == dropB {
		t.Fatalf("DROP INDEX fingerprints must name the index: %q %q", dropA, dropB)
	}
	if got := IndexFingerprint("VACUUM public.orders"); got != "" {
		t.Fatalf("non-index statement fingerprint = %q, want empty", got)
	}
	if got := IndexFingerprint(""); got != "" {
		t.Fatalf("empty SQL fingerprint = %q, want empty", got)
	}
}

func TestActionType(t *testing.T) {
	cases := map[string]string{
		"CREATE INDEX CONCURRENTLY i ON t (a)":                      "create_index",
		"create unique index concurrently i on t (a)":               "create_index",
		"DROP INDEX CONCURRENTLY i":                                 "drop_index",
		"REINDEX INDEX CONCURRENTLY i":                              "reindex",
		"VACUUM (ANALYZE) t":                                        "vacuum",
		"ANALYZE t":                                                 "analyze",
		"ALTER SYSTEM SET work_mem = '64MB'":                        "alter_system",
		"ALTER TABLE t SET (autovacuum_vacuum_scale_factor = 0.01)": "alter_table",
		"SELECT pg_terminate_backend(123)":                          "terminate_backend",
		"SELECT 1":                                                  "other",
		"":                                                          "other",
	}
	for sql, want := range cases {
		if got := ActionType(sql); got != want {
			t.Errorf("ActionType(%q) = %q, want %q", sql, got, want)
		}
	}
}

func TestBackoffDoublesAndCaps(t *testing.T) {
	if Backoff(0) <= 0 || Backoff(1) <= 0 {
		t.Fatalf("Backoff(0)=%v Backoff(1)=%v, want positive", Backoff(0), Backoff(1))
	}
	if Backoff(2) != 2*Backoff(1) || Backoff(3) != 2*Backoff(2) {
		t.Fatalf("Backoff does not double: %v %v %v", Backoff(1), Backoff(2), Backoff(3))
	}
	if Backoff(64) != Backoff(40) || Backoff(64) <= Backoff(3) {
		t.Fatalf("Backoff must cap without overflow: Backoff(40)=%v Backoff(64)=%v",
			Backoff(40), Backoff(64))
	}
}
