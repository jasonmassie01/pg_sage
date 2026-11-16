package workpack

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/llm"
)

// evid-02c: the model tier is a pure function of the local tier. It holds
// numeric statistics and redacted text only, its own content hash, and a
// leak guard refuses it when anything seeded in the local tier survives.
// No DB test here: model_leak_db_test.go seeds a real server.

func decodeModel(t *testing.T, m *Pack, name string, dst any) {
	t.Helper()
	if err := json.Unmarshal(m.File(name), dst); err != nil {
		t.Fatalf("model %s: %v (%s)", name, err, m.File(name))
	}
}

func TestForModel_NoSeededSecretReachesTheModelTier(t *testing.T) {
	m := mustForModel(t, modelFixture(t), ModelOptions{})
	all := tierBytes(m)
	for _, s := range seededSecrets {
		if bytes.Contains(all, []byte(s)) {
			t.Errorf("the model tier holds the seeded %q", s)
		}
	}
	if m.Manifest.Tier != TierModel || m.Format != Format {
		t.Fatalf("model manifest tier %q format %q", m.Manifest.Tier, m.Format)
	}
	sums := map[string]string{}
	for _, f := range m.Files {
		sums[f.Name] = sha256Hex(f.Data)
	}
	if m.ContentSHA256 != contentSHA256(sums) || m.Manifest.ContentSHA256 != m.ContentSHA256 {
		t.Fatalf("model content hash %s does not hash its files", m.ContentSHA256)
	}
}

func TestForModel_KeepsNumericStatisticsAndRedactedTexts(t *testing.T) {
	local := modelFixture(t)
	m := mustForModel(t, local, ModelOptions{})
	var st Stats
	decodeModel(t, m, fileStats, &st)
	c := st.Columns[0]
	if st.ValueSamples || c.MostCommonVals != nil || c.HistogramBounds != nil ||
		c.NullFrac != 0.25 || c.NDistinct != -1 || c.AvgWidth != 24 {
		t.Fatalf("model stats column = %+v (value_samples %v)", c, st.ValueSamples)
	}
	var tx Texts
	decodeModel(t, m, fileTexts, &tx)
	var got string
	for _, q := range tx.Texts {
		if q.QueryID == "1" {
			got = q.Text
		}
	}
	if want := "SELECT * FROM orders WHERE token = '?' AND card = ?"; got != want {
		t.Fatalf("model text of queryid 1 = %q, want %q", got, want)
	}
	var sc Schema
	decodeModel(t, m, fileSchema, &sc)
	if sc.Tables[0].Constraints[0] != "CHECK ((token <> '?'::text))" ||
		!strings.Contains(sc.Tables[0].Indexes[0], "(state = '?'::text)") {
		t.Fatalf("model schema = %+v", sc.Tables[0])
	}
	if m.ContentSHA256 == local.ContentSHA256 {
		t.Fatal("the model tier has the local tier's hash")
	}
}

func TestForModel_DropsPlanConditionsOwnerEmailAndSensitiveSettings(t *testing.T) {
	m := mustForModel(t, modelFixture(t), ModelOptions{})
	var pl Plans
	decodeModel(t, m, filePlans, &pl)
	if pl.Plans[0].Conditions != nil || pl.Plans[0].Shape == nil {
		t.Fatalf("model plan = %+v, want the shape without conditions", pl.Plans[0])
	}
	if bytes.Contains(m.File(fileContext), []byte("confirmed_by")) ||
		!bytes.Contains(m.File(fileContext), []byte("'?'")) {
		t.Fatalf("model context = %s", m.File(fileContext))
	}
	var se Settings
	decodeModel(t, m, fileSettings, &se)
	for _, s := range se.Settings {
		switch s.Name {
		case "primary_conninfo":
			if s.Value != valueRedacted || s.Changes[0].To != valueRedacted {
				t.Errorf("primary_conninfo in the model tier = %+v", s)
			}
		case "work_mem":
			if s.Value != "4096" {
				t.Errorf("work_mem in the model tier = %+v", s)
			}
		}
	}
}

func TestForModel_LocalTierIsUnchanged(t *testing.T) {
	local := modelFixture(t)
	before, body := local.ContentSHA256, append([]byte(nil), local.Body...)
	stats := append([]byte(nil), local.File(fileStats)...)
	mustForModel(t, local, ModelOptions{HashIdentifiers: true,
		PseudonymKey: testPseudonymKey()})
	if local.ContentSHA256 != before || !bytes.Equal(local.Body, body) ||
		!bytes.Equal(local.File(fileStats), stats) {
		t.Fatal("ForModel changed the local tier")
	}
}

// The test seam: with the redaction step bypassed, the guard refuses the
// tier and names the section and field, never the value.
func TestForModel_LeakGuardFiresWithRedactionBypassed(t *testing.T) {
	saved := modelRedact
	t.Cleanup(func() { modelRedact = saved })
	modelRedact = func(_ string, tree any) any { return tree }
	m, err := ForModel(modelFixture(t), ModelOptions{})
	if !errors.Is(err, ErrModelLeak) || m != nil {
		t.Fatalf("bypassed redaction: %v (tier %v), want ErrModelLeak and no tier", err, m)
	}
	for _, s := range seededSecrets {
		if strings.Contains(err.Error(), s) {
			t.Fatalf("the leak error names the value %q: %v", s, err)
		}
	}
	if !regexp.MustCompile(`[a-z]+\.json`).MatchString(err.Error()) {
		t.Fatalf("the leak error names no section: %v", err)
	}
}

func TestForModel_LeakGuardNamesEachSeededSection(t *testing.T) {
	saved := modelRedact
	t.Cleanup(func() { modelRedact = saved })
	for _, section := range []string{fileTexts, fileSchema, fileIndexes, filePlans,
		fileStats, fileContext} {
		modelRedact = func(name string, tree any) any {
			if name == section {
				return tree
			}
			return saved(name, tree)
		}
		_, err := ForModel(modelFixture(t), ModelOptions{})
		if !errors.Is(err, ErrModelLeak) || !strings.Contains(err.Error(), section) {
			t.Errorf("%s left unredacted: %v, want ErrModelLeak naming it", section, err)
		}
	}
}

func TestForModel_CredentialShapedTokensAreRefused(t *testing.T) {
	for _, token := range []string{"ghp_abcdefghijklmnopqrstuvwxyz0123",
		"AKIAABCDEFGHIJKLMNOP", "xoxb-1234567890-abcdefgh", "sk-live-abcdefghijkl",
		"-----BEGIN PRIVATE KEY-----"} {
		cat := fixtureCatalog()
		set := cat[fileSettings].(Settings)
		set.Settings[1].Value = token
		p := withSections(t, modelFixture(t), map[string]any{fileSettings: set})
		_, err := ForModel(p, ModelOptions{})
		if !errors.Is(err, ErrModelLeak) || !strings.Contains(err.Error(), fileSettings) ||
			strings.Contains(err.Error(), token) {
			t.Errorf("setting %q: %v, want ErrModelLeak naming settings.json only", token, err)
		}
	}
	// Words that only contain the prefixes are not credentials.
	cat := fixtureCatalog()
	set := cat[fileSettings].(Settings)
	set.Settings[1].Value = "disk-usage-monitoring xoxo AKIA"
	p := withSections(t, modelFixture(t), map[string]any{fileSettings: set})
	if _, err := ForModel(p, ModelOptions{}); err != nil {
		t.Fatalf("a harmless setting refused: %v", err)
	}
}

func TestForModel_NilPackIsRefused(t *testing.T) {
	if m, err := ForModel(nil, ModelOptions{}); err == nil || m != nil {
		t.Fatalf("ForModel(nil) = %v, %v", m, err)
	}
}

func TestForModel_EmptyPackGivesAnEmptyTierWithAValidHashAndNoCharts(t *testing.T) {
	w := fourWeeks()
	m := mustForModel(t, mustAssemble(t, synthRequest(w), history{}), ModelOptions{})
	if !ValidSHA(m.ContentSHA256) {
		t.Fatalf("empty model hash %q", m.ContentSHA256)
	}
	for _, f := range m.Files {
		if strings.HasPrefix(f.Name, chartDir) {
			t.Fatalf("an empty pack has chart %s", f.Name)
		}
	}
	var tx Texts
	decodeModel(t, m, fileTexts, &tx)
	if tx.Texts == nil || len(tx.Texts) != 0 {
		t.Fatalf("empty texts = %+v, want []", tx.Texts)
	}
}

func textPack(t *testing.T, text string) *Pack {
	t.Helper()
	w := fourWeeks()
	h := synthHistory(w)
	h.texts[1] = text
	return mustAssemble(t, synthRequest(w), h)
}

func modelText(t *testing.T, m *Pack, qid string) QueryText {
	t.Helper()
	var tx Texts
	decodeModel(t, m, fileTexts, &tx)
	for _, q := range tx.Texts {
		if q.QueryID == qid {
			return q
		}
	}
	t.Fatalf("queryid %s has no model text", qid)
	return QueryText{}
}

func TestForModel_TextOf2000CharactersKeptAnd2001Cut(t *testing.T) {
	for _, n := range []int{MaxModelTextChars, MaxModelTextChars + 1} {
		text := "SELECT * FROM orders WHERE " + strings.Repeat("é", n-27)
		q := modelText(t, mustForModel(t, textPack(t, text), ModelOptions{}), "1")
		runes := len([]rune(q.Text))
		switch {
		case n == MaxModelTextChars && (runes != n || q.Truncated):
			t.Errorf("%d characters: kept %d, truncated %v", n, runes, q.Truncated)
		case n > MaxModelTextChars && (runes != MaxModelTextChars || !q.Truncated):
			t.Errorf("%d characters: kept %d, truncated %v", n, runes, q.Truncated)
		}
	}
}

func TestForModel_OwnerNotesCappedAt300Characters(t *testing.T) {
	cat := fixtureCatalog()
	ctx := cat[fileContext].(Context)
	brief := ctx.Brief.(ContextFact)
	brief.Value = map[string]string{"growth": strings.Repeat("g", 301),
		"owner": strings.Repeat("o", 300)}
	ctx.Brief = brief
	p := withSections(t, modelFixture(t), map[string]any{fileContext: ctx})
	var got struct {
		Brief ContextFact `json:"brief"`
	}
	decodeModel(t, mustForModel(t, p, ModelOptions{}), fileContext, &got)
	if len(got.Brief.Value["growth"]) != MaxModelNoteChars ||
		len(got.Brief.Value["owner"]) != MaxModelNoteChars {
		t.Fatalf("model brief = %v", got.Brief.Value)
	}
}

func TestForModel_SanitizesNumbersWhateverTheProcessSwitch(t *testing.T) {
	defer llm.SetRedactNumericLiterals(true)
	llm.SetRedactNumericLiterals(false)
	q := modelText(t, mustForModel(t, modelFixture(t), ModelOptions{}), "1")
	if strings.Contains(q.Text, "4111111111111111") {
		t.Fatalf("model text kept the number with the switch off: %q", q.Text)
	}
	if llm.RedactNumericLiteralsEnabled() {
		t.Fatal("ForModel changed the process-wide switch")
	}
}

func TestForModel_BuiltTwiceAndConcurrentlyGivesTheSameBytes(t *testing.T) {
	local := modelFixture(t)
	opts := ModelOptions{HashIdentifiers: true, PseudonymKey: testPseudonymKey()}
	first := mustForModel(t, local, opts)
	var wg sync.WaitGroup
	got := make([]*Pack, 8)
	errs := make([]error, 8)
	for i := range got {
		wg.Add(1)
		go func() { defer wg.Done(); got[i], errs[i] = ForModel(local, opts) }()
	}
	wg.Wait()
	for i, m := range got {
		if errs[i] != nil || m.ContentSHA256 != first.ContentSHA256 ||
			!bytes.Equal(tierBytes(m), tierBytes(first)) {
			t.Fatalf("derivation %d: %v, hash %v want %s", i, errs[i], m, first.ContentSHA256)
		}
	}
}
