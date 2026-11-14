package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/cloudtel"
)

// apiauth-08b review: GET /api/v1/cloud-telemetry reads the process-wide
// cloud telemetry registry, not only the fleet, so filtering the fleet
// alone leaked every database's provider status to a scoped user. The
// statuses must cover the visible databases only and name no other member
// of a shared instance. No PostgreSQL: the registry is in memory.

func telemetryReadFixture(t *testing.T) http.Handler {
	t.Helper()
	shared := cloudtel.Unavailable("rvt-billing", "rds", "no AWS credentials")
	alone := cloudtel.Unavailable("rvt-search", "rds", "no AWS credentials")
	reg := map[string]*cloudtel.Runtime{"rvt-billing": shared, "rvt-ledger": shared,
		"rvt-search": alone}
	for db, rt := range reg {
		cloudtel.RegisterMember(db, rt)
	}
	t.Cleanup(func() {
		for db, rt := range reg {
			cloudtel.UnregisterMember(db, rt)
		}
	})
	src := &dbAuthzSource{}
	src.set(7, map[string]string{"rvt-billing": auth.RoleViewer})
	mux := http.NewServeMux()
	registerManagedCloudRoutes(mux, grantsFleet("rvt-billing", "rvt-ledger", "rvt-search"))
	return withDatabaseAuthz(&databaseAuthz{grants: src}, mux)
}

// telemetryMembers lists, per returned status, its database and members,
// keeping only this fixture's databases (other tests share the registry).
func telemetryMembers(t *testing.T, body string) map[string][]string {
	t.Helper()
	var out struct {
		Databases []cloudtel.Status `json:"databases"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode cloud telemetry: %v: %s", err, body)
	}
	got := map[string][]string{}
	for _, st := range out.Databases {
		if strings.HasPrefix(st.Database, "rvt-") {
			got[st.Database] = st.Databases
		}
	}
	return got
}

func TestDatabaseRead_CloudTelemetryScopedToVisibleDatabases(t *testing.T) {
	h := telemetryReadFixture(t)
	for _, q := range []string{"", "?database=all", "?database=rvt-billing"} {
		w := readGet(h, "/api/v1/cloud-telemetry"+q, dbViewer(7))
		if w.Code != http.StatusOK {
			t.Fatalf("scoped %q: %d %s, want 200", q, w.Code, w.Body)
		}
		if strings.Contains(w.Body.String(), "rvt-search") ||
			strings.Contains(w.Body.String(), "rvt-ledger") {
			t.Fatalf("scoped %q: body names a database outside the grant: %s", q, w.Body)
		}
		got := telemetryMembers(t, w.Body.String())
		if len(got) != 1 || strings.Join(got["rvt-billing"], ",") != "rvt-billing" {
			t.Fatalf("scoped %q: statuses %v, want only rvt-billing", q, got)
		}
	}
	hidden := readGet(h, "/api/v1/cloud-telemetry?database=rvt-search", dbViewer(7))
	unknown := readGet(h, "/api/v1/cloud-telemetry?database=rvt-nosuch", dbViewer(7))
	if hidden.Code != http.StatusNotFound || hidden.Body.String() != unknown.Body.String() {
		t.Fatalf("hidden %d %s vs unknown %d %s, want identical 404s", hidden.Code,
			hidden.Body, unknown.Code, unknown.Body)
	}
}

func TestDatabaseRead_CloudTelemetryUnscopedUnchanged(t *testing.T) {
	h := telemetryReadFixture(t)
	w := readGet(h, "/api/v1/cloud-telemetry?database=all", dbViewer(8))
	got := telemetryMembers(t, w.Body.String())
	if w.Code != http.StatusOK || len(got) != 2 ||
		strings.Join(got["rvt-billing"], ",") != "rvt-billing,rvt-ledger" ||
		strings.Join(got["rvt-search"], ",") != "rvt-search" {
		t.Fatalf("unscoped: %d statuses %v, want the shared instance and search", w.Code, got)
	}
}

func TestVisibleTelemetry_EmptyAndNil(t *testing.T) {
	if got := visibleTelemetry(nil, map[string]bool{"a": true}); got == nil || len(got) != 0 {
		t.Fatalf("nil statuses = %#v, want an empty non-nil list", got)
	}
	sts := []cloudtel.Status{{Database: "a"}, {Database: "b", Databases: []string{"b", "a"}}}
	if got := visibleTelemetry(sts, map[string]bool{}); len(got) != 0 {
		t.Fatalf("empty visible set = %v, want nothing", got)
	}
	got := visibleTelemetry(sts, map[string]bool{"a": true})
	if len(got) != 2 || got[0].Database != "a" || got[1].Database != "a" ||
		strings.Join(got[1].Databases, ",") != "a" {
		t.Fatalf("visible a = %+v, want both statuses narrowed to a", got)
	}
	if sts[1].Database != "b" || len(sts[1].Databases) != 2 {
		t.Fatalf("visibleTelemetry changed its input: %+v", sts[1])
	}
}
