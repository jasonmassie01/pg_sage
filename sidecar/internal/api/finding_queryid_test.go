package api

import (
	"encoding/json"
	"strconv"
	"testing"

	"github.com/pg-sage/sidecar/internal/cases"
	"github.com/pg-sage/sidecar/internal/fleet"
)

func TestFindingQueryIDNormalization(t *testing.T) {
	for _, text := range []string{"-5892612755791055559", "8379109690210330486", "-9223372036854775808", "9223372036854775807", "42"} {
		t.Run(text, func(t *testing.T) {
			var detail map[string]any
			if err := json.Unmarshal([]byte(`{"queryid":`+text+`,"delta_calls":1,"interval_seconds":59.998903955,"nested":{"estimate":2.5}}`), &detail); err != nil {
				t.Fatal(err)
			}
			normalizeFindingQueryID(detail, "query", "queryid:"+text)
			if detail["queryid"] != text {
				t.Fatalf("queryid=%v (%T), want exact string %s", detail["queryid"], detail["queryid"], text)
			}
			if detail["delta_calls"] != float64(1) || detail["interval_seconds"] != 59.998903955 || detail["nested"].(map[string]any)["estimate"] != 2.5 {
				t.Fatalf("non-identifier metrics changed: %v", detail)
			}
		})
	}
	t.Run("conflict", func(t *testing.T) {
		detail := map[string]any{"queryid": float64(41), "calls": float64(1)}
		normalizeFindingQueryID(detail, "query", "queryid:42")
		if detail["queryid"] != nil || detail["queryid_unavailable"] == nil || detail["calls"] != float64(1) {
			t.Fatalf("conflicting ID was not marked unavailable: %v", detail)
		}
	})
}

func TestFindingQueryIDsRemainExactAcrossAPIsAndCaseArtifacts(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	for _, text := range []string{"-5191732810777595558", "8379109690210330486"} {
		t.Run(text, func(t *testing.T) {
			var findingID int64
			err := pool.QueryRow(ctx, `INSERT INTO sage.findings
				(category,severity,object_type,object_identifier,title,detail,recommendation,status)
				VALUES ('slow_query','warning','query',$1,'Precision regression', $2::jsonb,
				 'Inspect the query plan.', 'open') RETURNING id`, "queryid:"+text,
				`{"queryid":`+text+`,"query":"SELECT payload FROM public.orders","mean_exec_ms":1201.920908}`).Scan(&findingID)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _, _ = pool.Exec(ctx, "DELETE FROM sage.findings WHERE id=$1", findingID) })
			single, err := queryFindingByID(ctx, pool, strconv.FormatInt(findingID, 10))
			if err != nil {
				t.Fatal(err)
			}
			assertExactFindingQueryID(t, single, text)
			page, err := queryFindingsPage(ctx, pool, fleet.FindingFilters{Status: "open", Category: "slow_query"},
				listPage{Limit: 1000}, "testdb", false)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, row := range page.rows {
				if row.row["id"] != strconv.FormatInt(findingID, 10) {
					continue
				}
				found = true
				assertExactFindingQueryID(t, row.row, text)
				projected := cases.ProjectFinding(sourceFindingFromMap(row.row))
				if len(projected.ActionCandidates) != 1 || projected.ActionCandidates[0].ScriptOutput == nil {
					t.Fatalf("missing investigation artifact: %+v", projected)
				}
				script := projected.ActionCandidates[0].ScriptOutput
				if script.Filename != "investigate_query_"+text+".sql" || script.PRTitle != "Investigate query plan for query "+text {
					t.Fatalf("rounded query artifact: %+v", script)
				}
			}
			if !found {
				t.Fatal("new query finding missing from list path")
			}
		})
	}
}

func assertExactFindingQueryID(t *testing.T, row map[string]any, want string) {
	t.Helper()
	detail, ok := row["detail"].(map[string]any)
	if !ok || detail["queryid"] != want || detail["mean_exec_ms"] != 1201.920908 {
		t.Fatalf("incorrect finding detail: %v", row["detail"])
	}
	encoded, err := json.Marshal(detail)
	if err != nil {
		t.Fatal(err)
	}
	var browser map[string]any
	if err := json.Unmarshal(encoded, &browser); err != nil || browser["queryid"] != want {
		t.Fatalf("JSON query ID did not remain exact: %s (%v)", encoded, err)
	}
}
