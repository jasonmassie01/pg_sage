package workpack

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/histstore"
	"github.com/pg-sage/sidecar/internal/testsupport/histfixture"
)

// evid-02c leak checks on a real server (the spec's CHECK): a secret-like
// literal in a statement text, a check constraint and a captured plan's
// filter, and a column whose most common value is a seeded marker, are in
// the local tier of a pack built with both opt-ins, and none of them is in
// the model tier's bytes. With the redaction step bypassed the leak guard
// refuses the tier, so the test can fail.

const (
	leakStatement  = "sk-test-stmt-9c0d1e2f3a4b"
	leakConstraint = "sk-test-check-5e6f7a8b9c0d"
	leakPlan       = "sk-test-plan-3a4b5c6d7e8f"
)

func seedLeaks(t *testing.T, p *histfixture.Pair) {
	t.Helper()
	ctx := context.Background()
	if _, err := p.Monitored.Exec(ctx, `ALTER TABLE public.orders ADD COLUMN token text
		DEFAULT 'sk-test-default-0f1e2d3c' CHECK (token <> '`+leakConstraint+`')`); err != nil {
		t.Fatalf("seed the constraint: %v", err)
	}
	at := fixFrom.Add((fixHours-1)*time.Hour + time.Minute)
	execHist(t, p.Monitored, `INSERT INTO sage.snapshots (collected_at, category, data{dbcol})
		VALUES ($1, 'queries', $2::jsonb{dbval})`, at,
		`[{"queryid":1,"query":"SELECT * FROM orders WHERE token = '`+leakStatement+
			`' AND id = 4111111111111111"},
		{"queryid":2,"query":"SELECT * FROM customers c JOIN orders o ON o.c = c.id"},
		{"queryid":3,"query":"INSERT INTO app.events VALUES ($1)"},
		{"queryid":4,"query":"select * from orders where x = $1"}]`)
	if _, err := p.Monitored.Exec(ctx, `INSERT INTO sage.explain_cache (captured_at, queryid,
		plan_json, source) VALUES ($1, 1, $2, 'auto_explain')`, fixFrom.Add(2*time.Hour),
		`[{"Plan": {"Node Type": "Seq Scan", "Relation Name": "orders", "Schema": "public",
		  "Filter": "(token = '`+leakPlan+`'::text)", "Plan Rows": 1}}]`); err != nil {
		t.Fatalf("seed the plan: %v", err)
	}
}

func leakPack(t *testing.T) *Pack {
	t.Helper()
	p := histfixture.NewPair(t)
	p.Switch(t, histstore.ModeMonitored)
	clearPacks(t, p)
	seedFixture(t, p.Monitored)
	seedCatalog(t, p)
	seedLeaks(t, p)
	req := sectionRequest()
	req.IncludeConditions, req.IncludeValueSamples = true, true
	pack, err := Build(context.Background(), p.Monitored, req)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return pack
}

func TestModelLeakDB_SeededSecretsAndMCVNeverReachTheModelTier(t *testing.T) {
	local := leakPack(t)
	localBytes := tierBytes(local)
	// The seeds reached the local tier: otherwise this test proves nothing.
	for _, s := range []string{leakStatement, leakConstraint, privacyMarker} {
		if !bytes.Contains(localBytes, []byte(s)) {
			t.Fatalf("the local tier does not hold the seeded %q", s)
		}
	}
	for _, opts := range []ModelOptions{{}, {HashIdentifiers: true,
		PseudonymKey: testPseudonymKey()}} {
		m, err := ForModel(local, opts)
		if err != nil {
			t.Fatalf("model tier (hash %v): %v", opts.HashIdentifiers, err)
		}
		all := tierBytes(m)
		for _, s := range []string{leakStatement, leakConstraint, leakPlan, privacyMarker,
			"sk-test-", "4111111111111111"} {
			if bytes.Contains(all, []byte(s)) {
				t.Errorf("hash %v: the model tier holds %q", opts.HashIdentifiers, s)
			}
		}
		if m.File(fileTexts) == nil || m.File(fileStats) == nil {
			t.Fatalf("the model tier lost a section: %v", m.Manifest.Files)
		}
	}
}

func TestModelLeakDB_BypassedRedactionIsRefusedNamingTheSection(t *testing.T) {
	local := leakPack(t)
	saved := modelRedact
	t.Cleanup(func() { modelRedact = saved })
	for _, section := range []string{fileTexts, fileSchema, fileStats} {
		modelRedact = func(name string, tree any) any {
			if name == section {
				return tree
			}
			return saved(name, tree)
		}
		m, err := ForModel(local, ModelOptions{})
		if !errors.Is(err, ErrModelLeak) || m != nil || !strings.Contains(err.Error(), section) {
			t.Errorf("%s unredacted: %v, want ErrModelLeak naming it", section, err)
		}
		if err != nil && strings.Contains(err.Error(), "sk-test") {
			t.Errorf("the leak error names the value: %v", err)
		}
	}
}
