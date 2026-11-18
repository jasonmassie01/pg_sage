package workpack

import (
	"errors"
	"strings"
	"testing"
)

// evid-02c review: the leak guard must refuse data of the local tier, not
// a model tier whose structure happens to spell a common literal. A
// statement filtering on 'active' or 'default' is in almost every
// workload, and pg_sage's own structure holds the same words: a column
// named is_active, pg_stat_activity's "active" state, a setting's
// "default" source.

func TestForModel_CommonLiteralsSpelledByStructureAreNotLeaks(t *testing.T) {
	w := fourWeeks()
	h := synthHistory(w)
	h.texts[1] = "SELECT * FROM orders WHERE is_active AND state = 'active' " +
		"AND token <> 'default'"
	cat := fixtureCatalog()
	sc := fixtureSchema()
	sc.Tables[0].Columns = append(sc.Tables[0].Columns,
		ColumnFact{Name: "is_active", Type: "boolean"})
	cat[fileSchema] = sc
	cat[fileConnections] = Connections{Hours: []ConnectionHour{{ID: "connections/hour/0",
		Snapshots: 1, States: map[string]float64{"active": 2, "idle": 1},
		Ages: valueUnknown}}}
	p := withSections(t, mustAssemble(t, synthRequest(w), h), cat)
	m, err := ForModel(p, ModelOptions{})
	if err != nil {
		t.Fatalf("a model tier whose structure spells 'active' and 'default' was "+
			"refused: %v", err)
	}
	texts := string(m.File(fileTexts))
	if strings.Contains(texts, "'active'") || strings.Contains(texts, "'default'") ||
		!strings.Contains(texts, "is_active") {
		t.Fatalf("texts.json = %s, want the literals redacted and the column kept", texts)
	}
	if !strings.Contains(string(m.File(fileSettings)), `"source":"default"`) {
		t.Fatalf("the fixture has no setting with source default: %s", m.File(fileSettings))
	}
}

// The same words are still leaks where data can flow: in a statement
// text, a definition or a note left unredacted.
func TestForModel_CommonLiteralStillRefusedWhereDataFlows(t *testing.T) {
	saved := modelRedact
	t.Cleanup(func() { modelRedact = saved })
	modelRedact = func(_ string, tree any) any { return tree }
	w := fourWeeks()
	h := synthHistory(w)
	h.texts[1] = "SELECT * FROM orders WHERE state = 'active'"
	p := withSections(t, mustAssemble(t, synthRequest(w), h), map[string]any{})
	_, err := ForModel(p, ModelOptions{})
	if !errors.Is(err, ErrModelLeak) || !strings.Contains(err.Error(), fileTexts) {
		t.Fatalf("an unredacted 'active' literal: %v, want ErrModelLeak naming %s", err,
			fileTexts)
	}
}

// A needle matches as a whole token: is_active and inactive do not hold
// the value active; a quoted, spaced or punctuated active does. A needle
// whose edges are not identifier characters matches anywhere.
func TestNeedleIn_WholeTokensOnly(t *testing.T) {
	g := leakGuard{needles: map[string][]needle{}}
	for _, v := range []string{"active", "-marker-"} {
		g.needles[v[:minNeedle]] = append(g.needles[v[:minNeedle]], needle{v, kindLiteral})
	}
	cases := map[string]bool{
		"is_active": false, "inactive": false, "actively": false, "active_users": false,
		"active": true, "state = 'active'": true, "(active)": true, "x.active": true,
		"a-marker-9z": true, "é-marker-9": true, "the active one": true,
	}
	for s, want := range cases {
		if _, got := g.leakIn(s, "text"); got != want {
			t.Errorf("leakIn(%q) = %v, want %v", s, got, want)
		}
	}
}

// Fixed-vocabulary fields and names are not searched for needles, map keys
// are structure; credential-shaped tokens are refused everywhere, keys
// included.
func TestLeakIn_VocabularyNamesAndKeysAreStructure(t *testing.T) {
	g := leakGuard{needles: map[string][]needle{"defaul": {{"default", kindLiteral}}},
		names: map[string]bool{"default": true}}
	if _, leak := g.leakIn("default", "source"); leak {
		t.Error("a setting source of default is a leak")
	}
	if _, leak := g.leakIn("default", "value"); leak {
		t.Error("a value that is exactly a name of the pack is a leak")
	}
	g.names = nil
	if _, leak := g.leakIn("default", "value"); !leak {
		t.Error("a needle in a data field is not a leak")
	}
	if _, leak := g.leakIn("sk-test-abcdefgh12", "source"); !leak {
		t.Error("a credential in a vocabulary field is not a leak")
	}
	tree := map[string]any{"states": map[string]any{"default": 1}}
	if err := checkLeaks(map[string]any{fileConnections: tree}, g); err != nil {
		t.Errorf("a map key spelling a needle refused the tier: %v", err)
	}
	tree = map[string]any{"states": map[string]any{"ghp_abcdefghijklmnopqrstuvwxyz0123": 1}}
	err := checkLeaks(map[string]any{fileConnections: tree}, g)
	if !errors.Is(err, ErrModelLeak) || !strings.Contains(err.Error(), "states (a key)") ||
		strings.Contains(err.Error(), "abcdef") {
		t.Errorf("a credential-shaped key: %v, want ErrModelLeak naming the key only", err)
	}
}
