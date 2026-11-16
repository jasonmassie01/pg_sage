package workpack

import (
	"bytes"
	"errors"
	"regexp"
	"strings"
	"testing"
)

// evid-02c HashIdentifiers: every schema, table, index and column name of
// the model tier becomes t_<10 hex> of an HMAC keyed by the install's
// pseudonym key; the map back to the names stays local.
// No concurrency test here beyond model_tier_test.go's: hashing reads only
// the key and the pack.

var pseudonymRE = regexp.MustCompile(`^t_[0-9a-f]{10}$`)

func hashed(t *testing.T, p *Pack) *Pack {
	t.Helper()
	return mustForModel(t, p, ModelOptions{HashIdentifiers: true,
		PseudonymKey: testPseudonymKey()})
}

func TestForModel_HashIdentifiersHidesEveryName(t *testing.T) {
	m := hashed(t, modelFixture(t))
	all := tierBytes(m)
	for _, name := range []string{"public", "orders", "customers", "o_live", "token",
		"state"} {
		if regexp.MustCompile(`\b` + name + `\b`).Match(all) {
			t.Errorf("the hashed model tier still names %q", name)
		}
	}
	if len(m.Pseudonyms) == 0 {
		t.Fatal("no pseudonym map")
	}
	back := map[string]bool{}
	for p, name := range m.Pseudonyms {
		if !pseudonymRE.MatchString(p) {
			t.Errorf("pseudonym %q is not t_<10 hex>", p)
		}
		back[name] = true
		if !bytes.Contains(all, []byte(p)) {
			t.Errorf("pseudonym %s of %s is not used", p, name)
		}
	}
	for _, name := range []string{"public", "orders", "o_live", "token"} {
		if !back[name] {
			t.Errorf("the pseudonym map has no entry for %q", name)
		}
	}
	for _, f := range m.Files {
		if f.Name == "pseudonyms.json" {
			t.Fatal("the pseudonym map is a model-tier file")
		}
	}
}

func TestForModel_PseudonymsAreStableAcrossPacksAndKeyed(t *testing.T) {
	a := hashed(t, modelFixture(t))
	w := fourWeeks()
	b := hashed(t, mustAssemble(t, synthRequest(w), synthHistory(w)))
	inverse := func(m *Pack) map[string]string {
		out := map[string]string{}
		for p, n := range m.Pseudonyms {
			out[n] = p
		}
		return out
	}
	if inverse(a)["orders"] == "" || inverse(a)["orders"] != inverse(b)["orders"] {
		t.Fatalf("orders is %q in one pack and %q in another", inverse(a)["orders"],
			inverse(b)["orders"])
	}
	other := mustForModel(t, modelFixture(t), ModelOptions{HashIdentifiers: true,
		PseudonymKey: bytes.Repeat([]byte{8}, 32)})
	if inverse(other)["orders"] == inverse(a)["orders"] {
		t.Fatal("two keys give the same pseudonym")
	}
}

func TestForModel_NamesDifferingOnlyInCaseGetDifferentPseudonyms(t *testing.T) {
	sc := fixtureSchema()
	upper := sc.Tables[0]
	upper.ID, upper.Table = "schema/table/public.Orders", "public.Orders"
	sc.Tables = append(sc.Tables, upper)
	m := hashed(t, withSections(t, modelFixture(t), map[string]any{fileSchema: sc}))
	var lower, mixed string
	for p, n := range m.Pseudonyms {
		switch n {
		case "orders":
			lower = p
		case "Orders":
			mixed = p
		}
	}
	if lower == "" || mixed == "" || lower == mixed {
		t.Fatalf("orders -> %q, Orders -> %q", lower, mixed)
	}
}

func TestForModel_HashIdentifiersWithoutAKeyIsRefused(t *testing.T) {
	for _, key := range [][]byte{nil, {}, bytes.Repeat([]byte{1}, 16)} {
		m, err := ForModel(modelFixture(t), ModelOptions{HashIdentifiers: true,
			PseudonymKey: key})
		if !errors.Is(err, ErrNoPseudonymKey) || m != nil ||
			!strings.Contains(err.Error(), "keyring") {
			t.Fatalf("key of %d bytes: %v (tier %v), want ErrNoPseudonymKey naming the "+
				"keyring", len(key), err, m)
		}
	}
}

func TestForModel_TogglingHashChangesTheModelHashNotTheLocal(t *testing.T) {
	local := modelFixture(t)
	before := local.ContentSHA256
	plain := mustForModel(t, local, ModelOptions{})
	withHash := hashed(t, local)
	if plain.ContentSHA256 == withHash.ContentSHA256 {
		t.Fatal("hashing identifiers did not change the model hash")
	}
	if local.ContentSHA256 != before {
		t.Fatal("hashing identifiers changed the local hash")
	}
	if len(plain.Pseudonyms) != 0 {
		t.Fatalf("an unhashed tier has pseudonyms %v", plain.Pseudonyms)
	}
}

// A name is replaced as a whole token, bare or double-quoted, wherever it
// stands (model-tier literals are already '?'); a longer word is kept.
func TestHashTokens_QuotedAndBareNamesOnly(t *testing.T) {
	ps := newPseudonymizer(testPseudonymKey(), map[string]bool{"orders": true,
		"My Table": true})
	got := ps.text(`SELECT "My Table".x FROM orders o WHERE ordersx = 'orders'`)
	p := ps.name("orders")
	if strings.Contains(got, "My Table") || strings.Count(got, p) != 2 ||
		!strings.Contains(got, "ordersx") || !strings.Contains(got, ps.name("My Table")+".x") {
		t.Fatalf("hashed text = %q", got)
	}
}
