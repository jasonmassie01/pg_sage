package whynot

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/policy"
)

// CHECK-02: every reason a decision can be recorded with maps to a link
// and either a fix or an explicit "no fix, because" (zero unmapped).
func TestEveryLedgerReasonMapsToALinkAndAFix(t *testing.T) {
	links := map[LinkName]bool{}
	for _, name := range Links() {
		links[name] = true
	}
	var unmapped []string
	for _, reason := range policy.LedgerReasons() {
		m, ok := mappingFor(string(reason))
		switch {
		case !ok:
			unmapped = append(unmapped, string(reason))
		case !links[m.link]:
			t.Errorf("%s maps to unknown link %q", reason, m.link)
		case m.sentence == "" || rawCode(m.sentence):
			t.Errorf("%s has no plain sentence: %q", reason, m.sentence)
		case !m.allows && m.fix.kind == "" && m.noFix == "":
			t.Errorf("%s fails with neither a fix nor a reason for none", reason)
		case m.fix.kind != "" && m.noFix != "":
			t.Errorf("%s has both a fix and a no-fix reason", reason)
		}
	}
	if len(unmapped) != 0 {
		t.Fatalf("ledger reasons without a why-not mapping: %v", unmapped)
	}
}

// The table names no reason the ledger does not know, so it cannot carry
// stale entries.
func TestReasonMappingsAreAllLedgerReasons(t *testing.T) {
	for reason := range reasonMappings {
		if !policy.IsLedgerReason(reason) {
			t.Errorf("mapping for %q, which is not a ledger reason", reason)
		}
	}
}

// Each failing reason, fed to the chain as a verdict, fails its own link
// and serves a resolved fix (never a template kind) or its no-fix reason.
func TestEveryFailingReasonFailsItsLinkInTheChain(t *testing.T) {
	for _, reason := range policy.LedgerReasons() {
		m, _ := mappingFor(string(reason))
		if m.allows {
			continue
		}
		in := approvalInputs()
		in.Verdicts = []Verdict{{Decision: "blocked", Reason: string(reason),
			Detail: "until 2026-11-18T01:00:00Z"}}
		link := linkOf(t, Chain(context.Background(), in), m.link)
		if link.Status != Fail || link.Reason != string(reason) {
			t.Errorf("%s: link %s = %+v", reason, m.link, link)
			continue
		}
		if link.Fix == nil && link.NoFix == "" {
			t.Errorf("%s: no fix and no reason", reason)
		}
		if link.Fix != nil && !servedKind(link.Fix.Kind) {
			t.Errorf("%s: fix kind %q is not served", reason, link.Fix.Kind)
		}
	}
}

// rawCode reports an underscore outside the product name pg_sage.
func rawCode(sentence string) bool {
	return strings.Contains(strings.ReplaceAll(sentence, "pg_sage", ""), "_")
}

func servedKind(kind FixKind) bool {
	switch kind {
	case FixGrantSQL, FixSetting, FixUILink, FixWaitUntil:
		return true
	}
	return false
}

func TestAllowingReasonsPassTheirLink(t *testing.T) {
	for _, reason := range policy.LedgerReasons() {
		m, _ := mappingFor(string(reason))
		if !m.allows {
			continue
		}
		in := approvalInputs()
		in.Window, in.TrustLevel = nil, ""
		in.Stop = Stop{}
		in.Verdicts = []Verdict{{Decision: "execute", Reason: string(reason)}}
		if link := linkOf(t, Chain(context.Background(), in), m.link); link.Status != Pass {
			t.Errorf("%s: link %s = %+v, want pass", reason, m.link, link)
		}
	}
}

func TestStatsGrantFixPerProvider(t *testing.T) {
	for provider, runner := range map[string]string{
		"rds": "rds_superuser", "aurora": "rds_superuser",
		"cloud-sql": "cloudsqlsuperuser", "alloydb": "alloydbsuperuser",
		"azure": "azure_pg_admin", "supabase": "the postgres role",
		"postgres": "a superuser", "": "a superuser", "nonsense": "a superuser",
	} {
		fix := statsGrantFix(`we"ird`, provider)
		if fix.Kind != FixGrantSQL || fix.Value != `GRANT pg_read_all_stats TO "we""ird";` ||
			!strings.Contains(fix.Note, runner) {
			t.Errorf("%q: %+v, want a quoted GRANT run by %s", provider, fix, runner)
		}
	}
	if fix := statsGrantFix("", "rds"); fix.Value != "GRANT pg_read_all_stats TO sage_agent;" {
		t.Errorf("empty role = %+v", fix)
	}
	if fix := extensionFix("rds"); !strings.Contains(fix.Note, "rds_superuser") ||
		fix.Value != "CREATE EXTENSION IF NOT EXISTS pg_stat_statements;" {
		t.Errorf("extension fix = %+v", fix)
	}
}

func TestWaitFromDetail(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	if fix := waitFromDetail("frees at 2026-11-17T10:00:00Z and 2026-11-17T13:00:00+01:00",
		now); fix != nil {
		t.Fatalf("past times = %+v, want nil", fix)
	}
	fix := waitFromDetail("until 2026-11-17T18:30:00.5Z on t", now)
	if fix == nil || fix.Value != "2026-11-17T18:30:00Z" {
		t.Fatalf("future time = %+v", fix)
	}
	if fix := waitFromDetail("", now); fix != nil {
		t.Fatalf("empty = %+v", fix)
	}
}
