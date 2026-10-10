package api

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/agentguard/classify"
)

// Spec §6.7: column classes are stored and edited by operators; scans only
// propose, and every signed-in role can ask "is this column classified,
// and as what?".

func newClassAPI(t *testing.T) (*http.ServeMux, string) {
	t.Helper()
	pool, ctx := phase2RequireDB(t)
	clean := func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM sage.facts WHERE fact_type = 'column_class'`)
	}
	clean()
	t.Cleanup(clean)
	sch := fmt.Sprintf("clsapi%06x", time.Now().UnixNano()&0xffffff)
	for _, s := range []string{`CREATE SCHEMA ` + sch,
		`CREATE TABLE ` + sch + `.users (id bigint, email text, api_key text, nick text)`} {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP SCHEMA `+sch+` CASCADE`)
	})
	mux := http.NewServeMux()
	registerAgentClassRoutes(mux, phase2MgrWithPool(pool))
	return mux, sch
}

func TestAgentClassRoutes_SetAndAsk(t *testing.T) {
	mux, sch := newClassAPI(t)
	base := "/api/v1/agent-classifications/testdb"
	set := map[string]string{"schema": sch, "table": "users", "column": "email",
		"class": "pii", "note": "contact"}
	if code, _ := doJSON(t, mux, viewerUser(), "PUT", base+"/columns", set); code !=
		http.StatusForbidden {
		t.Fatalf("viewer set: %d", code)
	}
	code, body := doJSON(t, mux, operatorUser(), "PUT", base+"/columns", set)
	if code != http.StatusOK || body["class"] != "pii" || body["status"] != "confirmed" ||
		body["decided_by"] != "op@x" {
		t.Fatalf("operator set: %d %v", code, body)
	}
	ask := base + "/column?schema=" + sch + "&table=users&column=email"
	code, body = doJSON(t, mux, viewerUser(), "GET", ask, nil)
	if code != http.StatusOK || body["class"] != "pii" || body["confirmed"] != true ||
		body["classified"] != true {
		t.Fatalf("ask email: %d %v", code, body)
	}
	code, body = doJSON(t, mux, viewerUser(), "GET",
		base+"/column?schema="+sch+"&table=users&column=nick", nil)
	if code != http.StatusOK || body["class"] != "" || body["classified"] != false {
		t.Fatalf("ask nick: %d %v", code, body)
	}
	if code, _ := doJSON(t, mux, viewerUser(), "GET",
		base+"/column?schema="+sch+"&table=users&column=ghost", nil); code !=
		http.StatusNotFound {
		t.Fatalf("unknown column: %d", code)
	}
	if code, _ := doJSON(t, mux, viewerUser(), "GET",
		"/api/v1/agent-classifications/nope/column?schema=a&table=b&column=c", nil); code !=
		http.StatusNotFound {
		t.Fatalf("unknown database: %d", code)
	}
}

func TestAgentClassRoutes_ScanListDecide(t *testing.T) {
	mux, sch := newClassAPI(t)
	base := "/api/v1/agent-classifications/testdb"
	if code, _ := doJSON(t, mux, viewerUser(), "POST", base+"/scan", nil); code !=
		http.StatusForbidden {
		t.Fatalf("viewer scan: %d", code)
	}
	var proposed float64
	cursor := ""
	for range 100 {
		code, body := doJSON(t, mux, operatorUser(), "POST", base+"/scan",
			map[string]string{"cursor": cursor})
		if code != http.StatusOK {
			t.Fatalf("scan: %d %v", code, body)
		}
		proposed += body["proposed"].(float64)
		if body["done"] == true {
			break
		}
		cursor, _ = body["next_cursor"].(string)
	}
	if proposed < 2 {
		t.Fatalf("scan proposed %v", proposed)
	}
	code, body := doJSON(t, mux, viewerUser(), "GET", base+"?status=proposed&limit=200", nil)
	items, _ := body["items"].([]any)
	if code != http.StatusOK || len(items) < 2 {
		t.Fatalf("list: %d %v", code, body)
	}
	var keyID float64
	var keyHash string
	for _, it := range items {
		m := it.(map[string]any)
		if m["schema"] == sch && m["column"] == "api_key" {
			keyID, keyHash = m["id"].(float64), m["hash"].(string)
			if m["class"] != string(classify.ClassSecret) {
				t.Fatalf("api_key proposed %v", m["class"])
			}
		}
	}
	if keyID == 0 {
		t.Fatalf("api_key not listed: %v", items)
	}
	decide := fmt.Sprintf("%s/%d/confirm", base, int64(keyID))
	if code, _ := doJSON(t, mux, operatorUser(), "POST", decide,
		map[string]string{"expect_hash": "stale"}); code != http.StatusConflict {
		t.Fatalf("stale hash: %d", code)
	}
	code, body = doJSON(t, mux, operatorUser(), "POST", decide,
		map[string]string{"expect_hash": keyHash, "note": "yes"})
	if code != http.StatusOK || body["status"] != "confirmed" {
		t.Fatalf("confirm: %d %v", code, body)
	}
	if code, _ := doJSON(t, mux, operatorUser(), "POST",
		fmt.Sprintf("%s/%d/reject", base, int64(keyID)), nil); code != http.StatusConflict {
		t.Fatalf("decide twice: %d", code)
	}
	if code, _ := doJSON(t, mux, operatorUser(), "POST", base+"/999999999/confirm",
		nil); code != http.StatusNotFound {
		t.Fatalf("unknown id: %d", code)
	}
}

func TestAgentClassRoutes_InvalidInput(t *testing.T) {
	mux, sch := newClassAPI(t)
	base := "/api/v1/agent-classifications/testdb"
	for name, body := range map[string]any{
		"bad class":     map[string]string{"schema": sch, "table": "users", "column": "id", "class": "public"},
		"unknown field": map[string]any{"schema": sch, "table": "users", "class": "pii", "x": 1},
		"not json":      "nope",
	} {
		if code, _ := doJSON(t, mux, operatorUser(), "PUT", base+"/columns", body); code !=
			http.StatusUnprocessableEntity {
			t.Errorf("%s: %d", name, code)
		}
	}
	if code, _ := doJSON(t, mux, operatorUser(), "PUT", base+"/columns",
		map[string]string{"schema": sch, "table": "users", "column": "ghost",
			"class": "pii"}); code != http.StatusNotFound {
		t.Errorf("unknown column: %d", code)
	}
	if code, _ := doJSON(t, mux, viewerUser(), "GET", base+"?status=maybe", nil); code !=
		http.StatusUnprocessableEntity {
		t.Errorf("bad status filter: %d", code)
	}
	if code, _ := doJSON(t, mux, viewerUser(), "GET", base+"?cursor=abc", nil); code !=
		http.StatusUnprocessableEntity {
		t.Errorf("bad cursor: %d", code)
	}
	if code, _ := doJSON(t, mux, operatorUser(), "POST", base+"/abc/confirm", nil); code !=
		http.StatusUnprocessableEntity {
		t.Errorf("bad id: %d", code)
	}
}
