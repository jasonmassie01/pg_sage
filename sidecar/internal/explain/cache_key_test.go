package explain

import (
	"crypto/sha256"
	"encoding/binary"
	"testing"
)

// No concurrent access tests: cacheKey is a pure function of its arguments
// and shares no state between calls.

// Statements that differ only inside a literal or a hint are different
// statements; the cache must never answer one with the other's plan.
func TestCacheKey_CollisionPairsDiffer(t *testing.T) {
	pairs := []struct{ name, a, b string }{
		{"line-comment marker inside a literal",
			"SELECT * FROM t WHERE a = '--x' AND b = 1",
			"SELECT * FROM t WHERE a = '--x' AND b = 2"},
		{"whitespace inside a literal",
			"SELECT * FROM t WHERE a = 'a  b'",
			"SELECT * FROM t WHERE a = 'a b'"},
		{"planner hint comment",
			"/*+ HashJoin(c o) */ SELECT * FROM c JOIN o USING (id)",
			"SELECT * FROM c JOIN o USING (id)"},
		{"seqscan hint",
			"/*+ SeqScan(t) */ SELECT * FROM t WHERE id = 1",
			"SELECT * FROM t WHERE id = 1"},
		{"block-comment marker inside a literal",
			"SELECT * FROM t WHERE a = '/*' AND b = 1 AND c = '*/'",
			"SELECT * FROM t WHERE a = '/*' AND b = 2 AND c = '*/'"},
	}
	for _, p := range pairs {
		t.Run(p.name, func(t *testing.T) {
			for _, planOnly := range []bool{false, true} {
				if cacheKey(p.a, nil, planOnly) == cacheKey(p.b, nil, planOnly) {
					t.Errorf("planOnly=%v: %q and %q share a cache key",
						planOnly, p.a, p.b)
				}
			}
		})
	}
}

func TestCacheKey_IdenticalInputsShareKey(t *testing.T) {
	body := "SELECT * FROM t WHERE a = $1 AND b = '--x'"
	params := []string{"7"}
	k1 := cacheKey(body, params, false)
	k2 := cacheKey(body, []string{"7"}, false)
	if k1 != k2 {
		t.Fatalf("identical body and params gave keys %d and %d", k1, k2)
	}
	if k1 == 0 {
		t.Fatal("cache key is zero")
	}
	if cacheKey(body, params, true) != cacheKey(body, params, true) {
		t.Error("plan-only key is not deterministic")
	}
}

func TestCacheKey_ModeSeparated(t *testing.T) {
	q := "SELECT * FROM orders"
	if cacheKey(q, nil, true) == cacheKey(q, nil, false) {
		t.Error("plan-only and ANALYZE share a cache key")
	}
}

func TestCacheKey_ParamsShapeTheKey(t *testing.T) {
	body := "SELECT $1, $2"
	cases := []struct {
		name string
		a, b []string
		same bool
	}{
		{"nil and empty slice are both no params", nil, []string{}, true},
		{"no params differs from one empty param", nil, []string{""}, false},
		{"one empty param differs from two", []string{""}, []string{"", ""}, false},
		{"param order matters", []string{"a", "b"}, []string{"b", "a"}, false},
		{"param boundary matters", []string{"ab", ""}, []string{"a", "b"}, false},
		{"different values differ", []string{"1"}, []string{"2"}, false},
		{"separator byte in a value", []string{"a\x00b"}, []string{"a", "b"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := cacheKey(body, tc.a, false) == cacheKey(body, tc.b, false)
			if got != tc.same {
				t.Errorf("same key = %v, want %v for %q vs %q", got, tc.same, tc.a, tc.b)
			}
		})
	}
}

// A parameter value must never be able to stand in for part of the body.
func TestCacheKey_BodyAndParamsDoNotBleed(t *testing.T) {
	if cacheKey("SELECT $1\x00a", nil, false) == cacheKey("SELECT $1", []string{"a"}, false) {
		t.Error("body bytes and a parameter produced the same key")
	}
	if cacheKey("SELECT 1", []string{"1"}, false) == cacheKey("SELECT 11", nil, false) {
		t.Error("a parameter appended to the body produced the same key")
	}
}

// The key hashes the exact body: the explainer has already trimmed the
// terminator and outer whitespace, and any other difference is kept.
func TestCacheKey_ExactBody(t *testing.T) {
	if cacheKey("SELECT 1", nil, false) == cacheKey("SELECT 1 -- note", nil, false) {
		t.Error("a trailing comment was ignored by the key")
	}
	if cacheKey("", nil, false) == cacheKey(" ", nil, false) {
		t.Error("empty and single-space bodies share a key")
	}
	// Invalid UTF-8 is hashed byte for byte and must not panic.
	if cacheKey("SELECT '\xff'", nil, false) == cacheKey("SELECT '\xfe'", nil, false) {
		t.Error("distinct invalid UTF-8 bodies share a key")
	}
}

// Callers choose the body and params, so the key must come from a
// collision-resistant hash: with FNV a caller could craft a statement
// whose key matches another statement and plant a plan for it.
func TestCacheKey_IsSHA256OfTheLayout(t *testing.T) {
	sum := sha256.Sum256([]byte("2:v27:analyze9:SELECT $11;1:7"))
	want := int64(binary.BigEndian.Uint64(sum[:8]))
	if got := cacheKey("SELECT $1", []string{"7"}, false); got != want {
		t.Fatalf("cacheKey = %d, want the SHA-256 prefix %d", got, want)
	}
}
