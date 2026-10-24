//go:build cgo

package reasoner

import (
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/changereview"
	"github.com/pg-sage/sidecar/internal/changereview/diffscan"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
)

// The opt-in live arm (CHECK-RSN-11): five fixture PRs against a real
// provider over schema rv. It runs only with PG_SAGE_LIVE_LLM=1 and a key
// (SAGE_LLM_API_KEY, else GEMINI_API_KEY; SAGE_LLM_ENDPOINT and
// SAGE_LLM_MODEL name another provider than Gemini's OpenAI-compatible
// endpoint) and is listed in .skip-allowlist as a live LLM test:
//
//	PG_SAGE_LIVE_LLM=1 SAGE_LLM_API_KEY=... go test -count=1 \
//	  -run TestLiveLLM ./internal/changereview/reasoner/

func liveLLM(t *testing.T) *llm.Client {
	t.Helper()
	if os.Getenv("PG_SAGE_LIVE_LLM") != "1" {
		t.Skip("set PG_SAGE_LIVE_LLM=1 and SAGE_LLM_API_KEY to run live LLM tests")
	}
	key := os.Getenv("SAGE_LLM_API_KEY")
	if key == "" {
		key = os.Getenv("GEMINI_API_KEY")
	}
	if key == "" {
		t.Skip("SAGE_LLM_API_KEY / GEMINI_API_KEY not set; skipping live LLM test")
	}
	endpoint := os.Getenv("SAGE_LLM_ENDPOINT")
	if endpoint == "" {
		endpoint = "https://generativelanguage.googleapis.com/v1beta/openai"
	}
	model := os.Getenv("SAGE_LLM_MODEL")
	if model == "" {
		model = "gemini-2.5-flash"
	}
	return llm.New(&config.LLMConfig{Enabled: true, Endpoint: endpoint, APIKey: key,
		Model: model, TimeoutSeconds: 60, TokenBudgetDaily: 2_000_000},
		func(string, string, ...any) {})
}

func newFile(path string, lines ...string) string {
	var b strings.Builder
	b.WriteString("diff --git a/" + path + " b/" + path + "\nnew file mode 100644\n" +
		"--- /dev/null\n+++ b/" + path + "\n")
	b.WriteString("@@ -0,0 +1," + strconv.Itoa(len(lines)) + " @@\n")
	for _, l := range lines {
		b.WriteString("+" + l + "\n")
	}
	return b.String()
}

var livePRs = []struct {
	name, tool, diff string
	extracted        bool // at least one accepted extracted statement
	none             bool // no accepted statement at all
}{
	{"rails rename", "rails", rvRailsDiff, false, false},
	{"django addfield not null", "django", newFile("orders/migrations/0008_flag.py",
		"from django.db import migrations, models",
		"class Migration(migrations.Migration):",
		"    dependencies = [('orders', '0007_note')]",
		"    operations = [migrations.AddField(model_name='rvorder', name='flag',",
		"        field=models.BooleanField())]",
		"# db_table of RvOrder is rv.rv_orders"), false, false},
	{"prisma new index", "prisma", newFile("prisma/schema.prisma",
		"model RvOrder {", "  id         BigInt   @id", "  status     String",
		"  created_at DateTime", "  @@index([status, created_at])",
		`  @@map("rv_orders") // schema rv`, "}"), false, false},
	{"n+1 in a controller", "rails", newFile("app/controllers/customers_controller.rb",
		"class CustomersController < ApplicationController",
		"  def show",
		"    @orders = RvOrder.where(customer_id: params[:id]) # table rv.rv_orders",
		"    @orders.each do |o|",
		"      o.items = RvItem.where(order_id: o.id).to_a # table rv.rv_items",
		"    end",
		"  end", "end"), true, false},
	{"readme only", "", newFile("README.md", "# Shop", "Run the migrations first."),
		false, true},
}

func TestLiveLLM_FixturePRs(t *testing.T) {
	model := liveLLM(t)
	for _, pr := range livePRs {
		t.Run(pr.name, func(t *testing.T) {
			e := newLiveEnv(t)
			cfg := config.DefaultChangeReviewConfig()
			r, err := New(Deps{Model: model, Tools: e.eng, Logf: t.Logf,
				Budget: changereview.NewBudget(e.f.Pool, int64(cfg.DailyTokensPerDatabase),
					int64(cfg.DailyTokensPerActor), nil)})
			if err != nil {
				t.Fatal(err)
			}
			s, err := changereview.New(changereview.Deps{Database: e.f.Name, Pool: e.f.Pool,
				Config: cfg, Analyzer: e.eng, Reasoner: r, Redact: diffscan.Redact,
				HunkFilter: diffscan.HunkFilter, Logf: t.Logf, ModelTimeout: 75 * time.Second})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Start(e.ctx); err != nil {
				t.Fatal(err)
			}
			got := e.submit(s, changereview.Request{Diff: pr.diff, MigrationTool: pr.tool})
			t.Logf("%s: %s/%s, %d statements, %d findings, %d dropped, %d tokens", pr.name,
				got.Verdict, got.ModelStatus, len(got.Statements), len(got.Findings),
				len(got.Dropped), got.Tokens)
			if got.ModelStatus != changereview.ModelOK {
				t.Fatalf("model status %q (transcript %+v)", got.ModelStatus, got.Transcript)
			}
			extracted, model := 0, 0
			for _, st := range got.Statements {
				if st.Origin == changereview.OriginModelExtracted {
					extracted++
				}
				if st.Origin != changereview.OriginSQLInput {
					model++
				}
			}
			if pr.extracted && extracted == 0 {
				t.Errorf("no accepted extracted statement; not determined %+v", got.NotDetermined)
			}
			if pr.none && model != 0 {
				t.Errorf("a README-only change has %d model statements", model)
			}
			held := map[string]bool{}
			for _, ev := range got.Evidence {
				held[ev.ID] = true
			}
			for _, f := range got.Findings {
				if f.Origin != changereview.OriginModel {
					continue
				}
				if len(f.EvidenceIDs) == 0 {
					t.Errorf("surviving finding %q cites nothing", f.Title)
				}
				for _, id := range f.EvidenceIDs {
					if !held[id] {
						t.Errorf("finding %q cites %s, which the review does not hold", f.Title,
							id)
					}
				}
			}
		})
	}
}
