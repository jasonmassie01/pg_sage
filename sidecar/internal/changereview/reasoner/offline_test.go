package reasoner

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/changereview"
)

// LLM off (CHECK-RSN-07): no request reaches a provider, the review is
// deterministic with model_status no_model, app-code changes are listed
// as not analyzed, and an ORM-only migration gets CR-20 with the exact
// export command of its tool.

func cr20(t *testing.T, res changereview.Reasoning) changereview.Finding {
	t.Helper()
	for _, f := range res.Findings {
		if f.RuleID == changereview.RuleNotAnalyzableSQL {
			return f
		}
	}
	t.Fatalf("no CR-20 in %+v", res.Findings)
	return changereview.Finding{}
}

func TestReason_NoModelGivesCR20WithTheExportCommand(t *testing.T) {
	tools := newFakeTools()
	r := newTestReasoner(t, Deps{Tools: tools})
	res := reason(t, r, testInput(railsRenameDiff, "rails"), noSQLAnalysis())
	if res.ModelStatus != changereview.ModelNoModel || res.Transcript != nil || res.Tokens != 0 {
		t.Fatalf("status %q transcript %+v tokens %d", res.ModelStatus, res.Transcript,
			res.Tokens)
	}
	f := cr20(t, res)
	rails, _ := changereview.LookupMigrationTool("rails")
	if f.Origin != changereview.OriginRule || f.Severity != changereview.SeverityWarning ||
		f.Kind != "not_analyzable_without_sql" || !strings.Contains(f.Text, rails.ExportCommand) ||
		!strings.Contains(f.Text, railsMigration) || len(f.EvidenceIDs) != 1 {
		t.Fatalf("CR-20 = %+v", f)
	}
	if !holds(res.Evidence, f.EvidenceIDs[0]) {
		t.Fatalf("CR-20 cites %v, not held in %v", f.EvidenceIDs, ids(res.Evidence))
	}
	if len(tools.inspections()) != 0 || len(tools.touching) != 0 {
		t.Fatal("the engine's tools ran without a model")
	}
	m, md := rendered(noSQLAnalysis(), res)
	if !hasRule(m.Findings, changereview.RuleNotAnalyzableSQL) ||
		m.Verdict != changereview.VerdictReview || m.ModelStatus != changereview.ModelNoModel {
		t.Fatalf("merged = %+v", m)
	}
	if !strings.Contains(md, "App-code changes were not analyzed") ||
		!strings.Contains(md, "CR-20") {
		t.Fatalf("the comment does not explain the LLM-off review:\n%s", md)
	}
}

func TestReason_DisabledClientMakesNoRequest(t *testing.T) {
	fake := newFakeLLM(t, emptySubmit())
	r := newTestReasoner(t, Deps{Model: fake.clientEnabled(false)})
	res := reason(t, r, testInput(railsRenameDiff, "rails"), noSQLAnalysis())
	if fake.calls() != 0 || res.ModelStatus != changereview.ModelNoModel {
		t.Fatalf("status %q after %d requests", res.ModelStatus, fake.calls())
	}
	cr20(t, res)
}

func TestReason_CR20ExportCommandPerTool(t *testing.T) {
	cases := []struct{ tool, path, line string }{
		{"django", "orders/migrations/0007_note.py",
			"migrations.AddField('order', 'note', models.TextField(null=False))"},
		{"alembic", "alembic/versions/3f2a_note.py", "op.add_column('orders', sa.Column('note'))"},
		{"prisma", "prisma/schema.prisma", "  note String"},
		{"knex", "migrations/20261023_note.js", "  return knex.schema.table('orders', t => t)"},
		{"sequelize", "migrations/20261023-note.js",
			"  await queryInterface.addColumn('orders', 'note', Sequelize.TEXT)"},
		{"typeorm", "src/migrations/1700000000000-Note.ts",
			"  await queryRunner.addColumn('orders', new TableColumn({ name: 'note' }))"},
	}
	for _, c := range cases {
		t.Run(c.tool, func(t *testing.T) {
			diff := "diff --git a/" + c.path + " b/" + c.path + "\nnew file mode 100644\n" +
				"--- /dev/null\n+++ b/" + c.path + "\n@@ -0,0 +1 @@\n+" + c.line + "\n"
			res := reason(t, newTestReasoner(t, Deps{}), testInput(diff, c.tool), noSQLAnalysis())
			tool, _ := changereview.LookupMigrationTool(c.tool)
			if f := cr20(t, res); !strings.Contains(f.Text, tool.ExportCommand) {
				t.Fatalf("CR-20 text %q lacks %q", f.Text, tool.ExportCommand)
			}
		})
	}
}

func TestReason_CR20InfersTheToolFromTheFiles(t *testing.T) {
	res := reason(t, newTestReasoner(t, Deps{}), testInput(railsRenameDiff, ""), noSQLAnalysis())
	rails, _ := changereview.LookupMigrationTool("rails")
	if f := cr20(t, res); !strings.Contains(f.Text, rails.ExportCommand) {
		t.Fatalf("undeclared tool: CR-20 text %q lacks the Rails export command", f.Text)
	}
	odd := "diff --git a/migrations/0001_x.ex b/migrations/0001_x.ex\nnew file mode 100644\n" +
		"--- /dev/null\n+++ b/migrations/0001_x.ex\n@@ -0,0 +1 @@\n+alter table(:orders)\n"
	if f := cr20(t, reason(t, newTestReasoner(t, Deps{}), testInput(odd, ""),
		noSQLAnalysis())); !strings.Contains(f.Text, "SQL") {
		t.Fatalf("an unknown tool's CR-20 gives no export hint: %q", f.Text)
	}
}

func TestReason_NoCR20WithSQLOrWithoutMigrationCode(t *testing.T) {
	withSQL := testInput(railsRenameDiff, "rails")
	withSQL.SQLFiles = []changereview.SQLFile{{Content: "ALTER TABLE orders ADD COLUMN n text"}}
	cases := map[string]struct {
		in changereview.Input
		a  changereview.Analysis
	}{
		"sql submitted":     {withSQL, safeAnalysis()},
		"app code only":     {testInput(controllerDiff, "rails"), noSQLAnalysis()},
		"sql migration":     {testInput(secretDiffSQL, "flyway"), noSQLAnalysis()},
		"nothing submitted": {testInput("", ""), noSQLAnalysis()},
	}
	for name, c := range cases {
		res := reason(t, newTestReasoner(t, Deps{}), c.in, c.a)
		if hasRule(res.Findings, changereview.RuleNotAnalyzableSQL) || len(res.Findings) != 0 {
			t.Errorf("%s: findings %+v, want none", name, res.Findings)
		}
		if res.ModelStatus != changereview.ModelNoModel {
			t.Errorf("%s: status %q", name, res.ModelStatus)
		}
	}
}

const secretDiffSQL = "diff --git a/migrations/V3__note.sql b/migrations/V3__note.sql\n" +
	"new file mode 100644\n--- /dev/null\n+++ b/migrations/V3__note.sql\n" +
	"@@ -0,0 +1 @@\n+ALTER TABLE orders ADD COLUMN note text;\n"

func TestReason_ModelThatDoesNotTranslateStillGetsCR20(t *testing.T) {
	fake := newFakeLLM(t, emptySubmit())
	res := reason(t, newTestReasoner(t, Deps{Model: fake.client()}),
		testInput(railsRenameDiff, "rails"), noSQLAnalysis())
	if res.ModelStatus != changereview.ModelOK {
		t.Fatalf("status %q", res.ModelStatus)
	}
	cr20(t, res)
}
