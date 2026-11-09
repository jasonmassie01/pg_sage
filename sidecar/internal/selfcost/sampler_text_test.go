package selfcost

import (
	"strings"
	"testing"
)

// State transition: an entry whose text pg_stat_statements cannot show
// (NULL: the query-text file failed to load, or the text was lost) is not
// classified. It is left out of that sample, as before the key cache, and
// told apart once its text reads, instead of being cached as not pg_sage's
// and missing from every later sample.
func TestSampler_UnreadableTextStaysUnclassified(t *testing.T) {
	db := newFakeDB("1.11")
	db.add(1, true, 5)
	db.add(2, false, 7)
	db.noText = map[int64]bool{1: true, 2: true}
	s := NewSampler(nil)
	if got := sampleOK(t, s, db); len(got) != 0 || db.count("texts") != 1 {
		t.Fatalf("unreadable texts: sample %+v after %d text reads, want none after one",
			got, db.count("texts"))
	}
	if got := sampleOK(t, s, db); len(got) != 0 || db.count("texts") != 2 {
		t.Fatalf("still unreadable: sample %+v after %d text reads, want a second read",
			got, db.count("texts"))
	}
	db.noText = nil
	db.set(1, 9)
	got := sampleOK(t, s, db)
	if db.count("texts") != 3 || len(got) != 1 || got[skey(1)].Calls != 9 ||
		!strings.Contains(got[skey(1)].Text, "sage_1") {
		t.Fatalf("readable again: sample %+v after %d text reads, want sage_1 at 9 calls",
			got, db.count("texts"))
	}
	if got := sampleOK(t, s, db); db.count("texts") != 3 || len(got) != 1 {
		t.Fatalf("classified keys: sample %+v, %d text reads, want no further read",
			got, db.count("texts"))
	}
}
