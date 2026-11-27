package api

import (
	"net/http"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/cloudtel"
)

// supabaseSource stands in for the sidecar's Supabase status adapter.
type supabaseSource struct{ database string }

func (s supabaseSource) Database() string { return s.database }

func (s supabaseSource) Status() cloudtel.Status {
	at := time.Now()
	return cloudtel.Status{Database: s.database, Provider: "supabase", Available: true,
		Withhold: []string{}, Sample: &cloudtel.Sample{Provider: "supabase", CollectedAt: at,
			MemoryTotalBytes: &cloudtel.Point{Value: 1 << 30, At: at}}}
}

// clouds-10 CHECK-02: /api/v1/cloud-telemetry lists Supabase status. No
// database: a nil manager accepts every database name.
func TestCloudTelemetryListsSupabaseStatus(t *testing.T) {
	src := supabaseSource{database: "supabase-db"}
	cloudtel.Register(src)
	t.Cleanup(func() { cloudtel.Unregister(src) })
	code, body := serveManaged(t, cloudTelemetryHandler(nil), "GET",
		"/api/v1/cloud-telemetry?database=supabase-db", nil, 0)
	dbs, _ := body["databases"].([]any)
	if code != http.StatusOK || len(dbs) != 1 {
		t.Fatalf("status = %d %v", code, body)
	}
	st, _ := dbs[0].(map[string]any)
	sample, _ := st["sample"].(map[string]any)
	mem, _ := sample["memory_total_bytes"].(map[string]any)
	if st["provider"] != "supabase" || st["database"] != "supabase-db" ||
		st["available"] != true || mem["value"] != float64(1<<30) {
		t.Fatalf("status = %v", st)
	}
	code, body = serveManaged(t, cloudTelemetryHandler(nil), "GET",
		"/api/v1/cloud-telemetry?database=other-db", nil, 0)
	if dbs, _ := body["databases"].([]any); code != http.StatusOK || len(dbs) != 0 {
		t.Fatalf("other database = %d %v", code, body)
	}
}
