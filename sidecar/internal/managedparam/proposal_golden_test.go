package managedparam

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/collector"
)

// market-12: adding the CloudNativePG builder must leave the RDS and Cloud
// SQL proposals byte-identical. The golden files were written by the code
// before the CNPG change (MP_UPDATE_GOLDEN=1 rewrites them).

// No concurrent access tests: Build is a pure function of its inputs.

func goldenProposals(t *testing.T) map[string]Proposal {
	t.Helper()
	running := &collector.PGSetting{Name: "shared_buffers", Setting: "131072", Unit: "8kB",
		Source: "configuration file"}
	out := map[string]Proposal{}
	build := func(name string, in Intent, target Target, r *collector.PGSetting) {
		p, err := Build(in, target, r)
		if err != nil {
			t.Fatalf("%s: Build: %v", name, err)
		}
		out[name] = p
	}
	build("rds_static", intent(t, "rds", "shared_buffers", "4GB"), rdsTarget(), running)
	build("rds_dynamic", intent(t, "rds", "work_mem", "64MB"), rdsTarget(), nil)
	build("rds_unresolved", intent(t, "rds", "work_mem", "64MB"),
		Target{Provider: "rds", Unresolved: "cloud telemetry is not configured"}, nil)
	sql := Target{Provider: "cloud-sql", Project: "orders-prod", InstanceID: "orders",
		Known: true, FlagsKnown: true, Flags: map[string]string{"work_mem": "4096",
			"log_min_duration_statement": "1000"}}
	build("cloudsql_known", intent(t, "cloud-sql", "work_mem", "64MB"), sql, nil)
	build("cloudsql_unknown", intent(t, "cloud-sql", "shared_buffers", "4GB"),
		Target{Provider: "cloud-sql", Unresolved: "no credentials"}, running)
	return out
}

func TestRDSAndCloudSQLProposalsAreUnchanged(t *testing.T) {
	update := os.Getenv("MP_UPDATE_GOLDEN") == "1"
	for name, p := range goldenProposals(t) {
		raw, err := json.MarshalIndent(p, "", "  ")
		if err != nil {
			t.Fatalf("%s: marshal: %v", name, err)
		}
		path := filepath.Join("testdata", "proposal_"+name+".golden.json")
		if update {
			if err := os.MkdirAll("testdata", 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, append(raw, '\n'), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: read golden: %v", name, err)
		}
		got := string(raw) + "\n"
		if strings.ReplaceAll(string(want), "\r\n", "\n") != got {
			t.Errorf("%s proposal changed:\n%s\nwant\n%s", name, got, want)
		}
	}
}
