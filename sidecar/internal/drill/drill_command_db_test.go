package drill

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/clone"
	"github.com/pg-sage/sidecar/internal/testsupport/restorecmd"
)

// drill-01c integration: the drill engine on a self-managed restore
// command. The command is this test binary acting as restorecmd; it
// "restores" by printing the rig's clone database on the second cluster.
// No concurrent access tests here: two drills' processes are covered in
// internal/clone (TestCommandDB_ConcurrentDrillsSeeTheirOwnDrillID).

func (r *drillRig) commandDrill(t *testing.T, restoreMode string) (Drill, string) {
	t.Helper()
	work := t.TempDir()
	cmd, err := clone.NewCommandProvider(clone.CommandConfig{
		RestoreArgv: restorecmd.Argv(restoreMode), DestroyArgv: restorecmd.Argv("destroy-ok"),
		EnvAllow: []string{restorecmd.EnvDSN}, WorkDir: work, MaxDuration: 2 * time.Minute,
		SourceDSN: r.source.Config().ConnString(),
		LookupEnv: func(name string) (string, bool) {
			if name == restorecmd.EnvDSN {
				return r.cloneDSN, true
			}
			return "", false
		}})
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewReceiptedProvider(cmd, clone.NewReceiptStore(r.control), ProviderConfig{
		Adapter: clone.CommandAdapter, Scope: "command/drill-test",
		Deployment:    func(context.Context) (string, error) { return testDeployment, nil },
		Budget:        Budget{MaxDuration: 10 * time.Minute},
		AllowFullCopy: true, MaxCreatesPerHour: 60})
	if err != nil {
		t.Fatal(err)
	}
	d := r.drill(t, "command")
	d.Provider, d.ProviderName, d.Kind = p, clone.CommandAdapter, clone.KindFullCopy
	return d, work
}

func TestRunDB_RestoreCommandDrillPasses(t *testing.T) {
	r := newRig(t, fixtureSQL, true, 90*time.Second)
	d, work := r.commandDrill(t, "ok")
	res, err := d.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v (failure %q)", err, res.Failure)
	}
	if !res.Passed || res.Failure != "" {
		t.Fatalf("drill failed: %q %+v", res.Failure, res.Invariants)
	}
	row := onlyRow(t, r.control)
	if !row.passed || row.provider != clone.CommandAdapter || row.kind != "full_copy" {
		t.Fatalf("stored row = %+v, want a passed restore_command full_copy drill", row)
	}
	if row.rpo == nil || row.rto == nil {
		t.Fatalf("rpo %v rto %v: both are measured on a command restore", row.rpo, row.rto)
	}
	if got := r.receipts(t); got[clone.StatusDestroyed] != 1 || len(got) != 1 {
		t.Fatalf("receipts = %v, want one destroyed", got)
	}
	entries, err := os.ReadDir(work)
	if err != nil || len(entries) != 1 ||
		!strings.HasPrefix(entries[0].Name(), "destroyed-clone-sage-drill-") {
		t.Fatalf("work dir %v, %v: want the destroy program's record", entries, err)
	}
	raw, _ := os.ReadFile(filepath.Join(work, entries[0].Name()))
	if !strings.HasPrefix(string(raw), "clone-sage-drill-") {
		t.Fatalf("destroy saw SAGE_CLONE_ID %q", raw)
	}
}

func TestRunDB_RestoreCommandFailureStoresNoSecret(t *testing.T) {
	r := newRig(t, fixtureSQL, true, 90*time.Second)
	d, _ := r.commandDrill(t, "fail")
	res, err := d.Run(context.Background())
	if err == nil || res.Passed {
		t.Fatalf("Run = %+v, %v; want a failed drill", res, err)
	}
	row := onlyRow(t, r.control)
	if row.passed || !strings.Contains(row.failure, "repo locked") {
		t.Fatalf("stored failure %q, want the stderr tail", row.failure)
	}
	for _, secret := range []string{"hunter2", "backup:"} {
		if strings.Contains(row.failure, secret) {
			t.Fatalf("stored failure %q leaks %q", row.failure, secret)
		}
	}
	if got := r.receipts(t); got[clone.StatusFailed] != 1 {
		t.Fatalf("receipts = %v, want the create closed failed", got)
	}
}
