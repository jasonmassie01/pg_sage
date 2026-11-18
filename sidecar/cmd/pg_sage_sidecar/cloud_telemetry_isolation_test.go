package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/cloudtel"
	"github.com/pg-sage/sidecar/internal/config"
)

// drill-01b: backup_shares_delete_path, deletion_protection_off and
// pitr_window_below_policy from the provider's backup posture. Unknown
// evidence is "not evaluated": the category is absent, so an open finding
// stays open.

// No concurrent access tests: isolationFindings is a pure function of its
// arguments; the runtime's posture copy is covered in cloudtel.

var isoPolicy = config.BackupConfig{MaxAgeHours: 26, MaxArchiveGapMinutes: 15,
	MinRetentionDays: 7, MinPITRHours: 24}

const isoResource = "rds:us-east-1/orders"

func survives(v bool, evidence string) *cloudtel.BackupPosture {
	return &cloudtel.BackupPosture{SurvivesDelete: &v, IsolationEvidence: evidence}
}

func oneFinding(t *testing.T, got map[string][]analyzer.Finding,
	category string) analyzer.Finding {
	t.Helper()
	list, ok := got[category]
	if !ok || len(list) != 1 {
		t.Fatalf("%s = %v (evaluated %v), want one finding", category, list, ok)
	}
	return list[0]
}

func TestDeletePathOpensOnAutomatedOnly(t *testing.T) {
	got := isolationFindings(survives(false, "only automated backups"), isoResource,
		isoPolicy, prodNo)
	f := oneFinding(t, got, categoryBackupDeletePath)
	if f.Severity != "warning" || f.ObjectIdentifier != "backup:"+isoResource ||
		f.Detail["isolation_evidence"] != "only automated backups" ||
		f.Detail["resource"] != isoResource {
		t.Fatalf("finding = %+v", f)
	}
	if !strings.Contains(f.Recommendation, "manual snapshot") {
		t.Fatalf("recommendation %q does not say how to fix it", f.Recommendation)
	}
}

// On production with the agent gate on, a shared delete path is critical:
// pg_sage cannot see which agent holds a delete-capable cloud credential.
func TestDeletePathIsCriticalOnProduction(t *testing.T) {
	got := isolationFindings(survives(false, "e"), isoResource, isoPolicy, prodYes)
	if f := oneFinding(t, got, categoryBackupDeletePath); f.Severity != "critical" {
		t.Fatalf("production severity = %q, want critical", f.Severity)
	}
	got = isolationFindings(survives(false, "e"), isoResource, isoPolicy, prodUnknown)
	if f := oneFinding(t, got, categoryBackupDeletePath); f.Severity != "warning" {
		t.Fatalf("unknown environment severity = %q, want warning", f.Severity)
	}
}

func TestDeletePathResolvesWhenABackupSurvives(t *testing.T) {
	got := isolationFindings(survives(true, "manual snapshot"), isoResource, isoPolicy,
		prodYes)
	if list, ok := got[categoryBackupDeletePath]; !ok || len(list) != 0 {
		t.Fatalf("survives = %v (evaluated %v), want an empty, evaluated list", list, ok)
	}
}

// An operator-declared external backup source resolves the finding even
// when the provider shows only automated backups.
func TestDeletePathExternalSourceResolves(t *testing.T) {
	p := isoPolicy
	p.ExternalSource = "AWS Backup vault in the audit account"
	got := isolationFindings(survives(false, "e"), isoResource, p, prodYes)
	if list, ok := got[categoryBackupDeletePath]; !ok || len(list) != 0 {
		t.Fatalf("external source = %v (evaluated %v), want resolved", list, ok)
	}
}

// Nil/empty: no posture, or one with nothing known, evaluates nothing.
func TestIsolationUnknownIsNotEvaluated(t *testing.T) {
	if got := isolationFindings(nil, isoResource, isoPolicy, prodYes); len(got) != 0 {
		t.Fatalf("nil posture evaluated %v", got)
	}
	got := isolationFindings(&cloudtel.BackupPosture{IsolationUnavailable: "403"},
		isoResource, isoPolicy, prodYes)
	if len(got) != 0 {
		t.Fatalf("an unknown posture evaluated %v", got)
	}
}

func TestDeletionProtectionOffOnProduction(t *testing.T) {
	off, on := false, true
	got := isolationFindings(&cloudtel.BackupPosture{DeletionProtection: &off}, isoResource,
		isoPolicy, prodYes)
	f := oneFinding(t, got, categoryDeletionProtection)
	if f.Severity != "warning" || f.ObjectIdentifier != "backup:"+isoResource {
		t.Fatalf("finding = %+v", f)
	}
	for _, tc := range []struct {
		dp   *bool
		prod productionState
	}{{&on, prodYes}, {&off, prodNo}} {
		got := isolationFindings(&cloudtel.BackupPosture{DeletionProtection: tc.dp},
			isoResource, isoPolicy, tc.prod)
		if list, ok := got[categoryDeletionProtection]; !ok || len(list) != 0 {
			t.Fatalf("dp %v prod %v: %v (evaluated %v), want resolved", *tc.dp, tc.prod,
				list, ok)
		}
	}
	// Unknown environment or unknown protection: not evaluated.
	got = isolationFindings(&cloudtel.BackupPosture{DeletionProtection: &off}, isoResource,
		isoPolicy, prodUnknown)
	if _, ok := got[categoryDeletionProtection]; ok {
		t.Fatal("deletion protection evaluated without knowing the environment")
	}
	got = isolationFindings(&cloudtel.BackupPosture{}, isoResource, isoPolicy, prodYes)
	if _, ok := got[categoryDeletionProtection]; ok {
		t.Fatal("deletion protection evaluated without knowing the flag")
	}
}

// Boundary: a 24 h window passes a 24 h policy; 23 h 59 m fails.
func TestPITRWindowBoundary(t *testing.T) {
	for _, tc := range []struct {
		window time.Duration
		open   bool
	}{{24 * time.Hour, false}, {23*time.Hour + 59*time.Minute, true}} {
		w := tc.window
		got := isolationFindings(&cloudtel.BackupPosture{PITRWindow: &w}, isoResource,
			isoPolicy, prodNo)
		list, ok := got[categoryPITRWindow]
		if !ok || (len(list) == 1) != tc.open {
			t.Fatalf("window %v: %v (evaluated %v), want open=%v", w, list, ok, tc.open)
		}
		if tc.open && (list[0].Severity != "warning" ||
			list[0].Detail["min_pitr_hours"] != 24 ||
			list[0].Detail["pitr_window_minutes"] != int64(1439)) {
			t.Fatalf("finding = %+v", list[0])
		}
	}
}

func TestPITRWindowUnknownOrOff(t *testing.T) {
	got := isolationFindings(&cloudtel.BackupPosture{}, isoResource, isoPolicy, prodNo)
	if _, ok := got[categoryPITRWindow]; ok {
		t.Fatal("PITR window evaluated without a window")
	}
	w := time.Hour
	p := isoPolicy
	p.MinPITRHours = 0
	got = isolationFindings(&cloudtel.BackupPosture{PITRWindow: &w}, isoResource, p, prodNo)
	if list, ok := got[categoryPITRWindow]; !ok || len(list) != 0 {
		t.Fatalf("policy 0 (off): %v (evaluated %v), want resolved", list, ok)
	}
}

// State transitions of the production lookup: no agent gate is unknown; a
// gate without environment bindings is production (the safe side).
func TestProductionOfFollowsTheAgentGate(t *testing.T) {
	prev := agentGate.Load()
	t.Cleanup(func() { agentGate.Store(prev) })
	agentGate.Store(nil)
	if got := productionOf(context.Background(), "orders"); got != prodUnknown {
		t.Fatalf("no agent gate: %v, want unknown", got)
	}
	agentGate.Store(&agentGateState{})
	if got := productionOf(context.Background(), "orders"); got != prodYes {
		t.Fatalf("gate without environment bindings: %v, want production", got)
	}
	if !agentGateNeedsDrill(context.Background(), "orders") {
		t.Fatal("agentGateNeedsDrill no longer follows productionOf")
	}
}
