package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// ux-08a: GET /api/v1/whynot answers "why has nothing been done here?" as
// one chain from observed to emergency stop. These tests need no
// database; whynot_db_test.go runs the chain on a real server.

type whyNotLinkBody struct {
	Name      string            `json:"name"`
	Status    string            `json:"status"`
	Sentence  string            `json:"sentence"`
	Fix       map[string]string `json:"fix"`
	NoFix     string            `json:"no_fix"`
	Reason    string            `json:"reason"`
	Error     string            `json:"error"`
	FirstFail bool              `json:"first_fail"`
}

type whyNotBody struct {
	Database  string           `json:"database"`
	FindingID string           `json:"finding_id"`
	Object    string           `json:"object"`
	Category  string           `json:"category"`
	FirstFail string           `json:"first_fail"`
	Failing   []string         `json:"failing"`
	Links     []whyNotLinkBody `json:"links"`
}

func (b whyNotBody) link(t *testing.T, name string) whyNotLinkBody {
	t.Helper()
	for _, link := range b.Links {
		if link.Name == name {
			return link
		}
	}
	t.Fatalf("no %s link in %+v", name, b.Links)
	return whyNotLinkBody{}
}

func getWhyNot(t *testing.T, mux http.Handler, query string) (int, whyNotBody, string) {
	t.Helper()
	rec := requestPolicy(mux, http.MethodGet, "/api/v1/whynot?"+query, "", testOperatorUser())
	var body whyNotBody
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode %s: %v", rec.Body, err)
		}
	}
	return rec.Code, body, rec.Body.String()
}

// poollessFleet registers "prod" with no pool and no executor.
func poollessFleet() *fleet.DatabaseManager {
	mgr := fleet.NewManager(&config.Config{Mode: "fleet"})
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "prod",
		Config: config.DatabaseConfig{Name: "prod"}, Status: &fleet.InstanceStatus{}})
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "other",
		Config: config.DatabaseConfig{Name: "other"}, Status: &fleet.InstanceStatus{}})
	return mgr
}

// Invalid input is refused before any database is read, naming the
// parameter at fault.
func TestWhyNotInvalidInput(t *testing.T) {
	mux := decisionsMux(poollessFleet())
	for query, field := range map[string]string{
		"database=prod&finding_id=3&object=public.t":             "finding_id",
		"database=prod&finding_id=abc":                           "finding_id",
		"database=prod&finding_id=0":                             "finding_id",
		"database=prod&finding_id=-1":                            "finding_id",
		"database=prod&object=public":                            "object",
		"database=prod&object=public.t%3Bdrop":                   "object",
		"database=prod&object=a.b.c":                             "object",
		"database=prod&object=" + strings.Repeat("x", 64) + ".t": "object",
		"database=prod&category=Bad%20Category":                  "category",
		"database=prod&category=" + strings.Repeat("a", 65):      "category",
		"database=prod&finding_id=3&category=unused_index":       "finding_id",
		"database=bad%20name":                                    "database",
	} {
		code, _, raw := getWhyNot(t, mux, query)
		if code != http.StatusBadRequest || !strings.Contains(raw, field) {
			t.Errorf("%s: %d %s, want 400 naming %s", query, code, raw, field)
		}
	}
}

// Nil/empty: no database named in a fleet of two is 400; an unknown
// database, or one without a pool, is 404.
func TestWhyNotDatabaseResolution(t *testing.T) {
	mux := decisionsMux(poollessFleet())
	for query, want := range map[string]int{
		"":                http.StatusBadRequest,
		"database=nope":   http.StatusNotFound,
		"database=prod":   http.StatusNotFound,
		"database=all":    http.StatusBadRequest,
		"database=other&": http.StatusNotFound,
	} {
		if code, _, raw := getWhyNot(t, mux, query); code != want {
			t.Errorf("%q: %d %s, want %d", query, code, raw, want)
		}
	}
	if code, _, _ := getWhyNot(t, decisionsMux(nil), "database=prod"); code != http.StatusNotFound {
		t.Errorf("no fleet manager: %d, want 404", code)
	}
}

// Authorization: like the policy explain route, a viewer gets 403 and an
// agent bearer token 401.
func TestWhyNotRequiresOperator(t *testing.T) {
	mux := decisionsMux(poollessFleet())
	rec := requestPolicy(mux, http.MethodGet, "/api/v1/whynot?database=prod", "",
		testViewerUser())
	if rec.Code != http.StatusForbidden {
		t.Errorf("viewer: %d %s, want 403", rec.Code, rec.Body)
	}
	if rec := serve(mux, newAgentTokenRequest("/api/v1/whynot?database=prod")); rec.Code !=
		http.StatusUnauthorized {
		t.Errorf("agent token: %d, want 401", rec.Code)
	}
}

func TestWhyNotObjectAndCategoryShapes(t *testing.T) {
	for object, ok := range map[string]bool{
		"public.orders": true, "app_1.idx_orders$2": true, "_s.t": true,
		"public.": false, ".t": false, "1s.t": false, `public."T"`: false, "": false,
	} {
		if got := validWhyNotObject(object); got != ok {
			t.Errorf("validWhyNotObject(%q) = %v, want %v", object, got, ok)
		}
	}
	for category, ok := range map[string]bool{
		"unused_index": true, "schema_lint:fk_without_index": true, "a.b-c": true,
		"Unused": false, "": false, "x y": false, "_lead": false,
	} {
		if got := validWhyNotCategory(category); got != ok {
			t.Errorf("validWhyNotCategory(%q) = %v, want %v", category, got, ok)
		}
	}
}
