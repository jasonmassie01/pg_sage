package modelpass

import (
	"strings"
	"testing"
)

// Hashed identifiers (D3, review.hash_identifiers): the model reads
// pseudonyms and the person reads names. A candidate never shows its SQL.
// No concurrent access tests: a renamer is immutable once built.

func TestRenamer_HidesAndShowsNames(t *testing.T) {
	r := newRenamer(map[string]string{"t_0123456789": "orders",
		"t_abcdefabcd": "orders_archive"})
	if got := r.hide("public.orders_archive and public.orders"); got !=
		"public.t_abcdefabcd and public.t_0123456789" {
		t.Fatalf("hide = %q, want the longer name swapped whole", got)
	}
	if got := r.show("drop t_abcdefabcd"); got != "drop orders_archive" {
		t.Fatalf("show = %q", got)
	}
	none := newRenamer(nil)
	if none.hide("orders") != "orders" || none.show("t_0123456789") != "t_0123456789" {
		t.Fatal("a tier without pseudonyms changed a name")
	}
}

func TestBuildSeed_CandidatesCarryNoSQL(t *testing.T) {
	in := input(t, "db1")
	seed, views, err := buildSeed(in)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if len(seed) != len(in.Candidates)+len(in.Pack.IDs()) || len(views) != 4 {
		t.Fatalf("seed = %d entries, views %d", len(seed), len(views))
	}
	for i, c := range in.Candidates {
		e := seed[i]
		if e.ID != candidatePrefix+c.ID || strings.Contains(e.Text, "DROP") ||
			strings.Contains(e.Text, "CREATE") || strings.Contains(e.Text, c.Reason) {
			t.Errorf("candidate evidence %d = %+v, want no SQL or prose", i, e)
		}
	}
	if _, _, err := buildSeed(Input{}); err == nil {
		t.Fatal("a seed was built without a pack")
	}
}
