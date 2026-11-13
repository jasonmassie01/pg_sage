package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/store"
)

// security-09b: the managed-database API takes a write-only action
// identity (action_username, action_password) and shows only whether one
// is configured and its role name.
// No concurrent access tests here: the handlers keep no shared state; the
// store's concurrent-update test covers the row.

func strPtr(s string) *string { return &s }

func TestDBCreateRequest_ActionFields(t *testing.T) {
	cases := []struct {
		name string
		req  dbCreateRequest
		want *store.ActionCredentialInput
	}{
		{"absent leaves it", dbCreateRequest{}, nil},
		{"empty username removes", dbCreateRequest{ActionUsername: strPtr("")},
			&store.ActionCredentialInput{}},
		{"pair sets", dbCreateRequest{ActionUsername: strPtr("actor"), ActionPassword: "pw"},
			&store.ActionCredentialInput{Username: "actor", Password: "pw"}},
		{"password alone is passed on to be refused", dbCreateRequest{ActionPassword: "pw"},
			&store.ActionCredentialInput{Password: "pw"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.req.toInput().Action
			if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
				t.Fatalf("Action = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestDBCreateRequest_DecodesActionFields(t *testing.T) {
	var req dbCreateRequest
	body := `{"name":"n","action_username":"actor","action_password":"pw"}`
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	if req.ActionUsername == nil || *req.ActionUsername != "actor" ||
		req.ActionPassword != "pw" {
		t.Fatalf("decoded %+v", req)
	}
}

func TestDBRecordToMap_ActionIdentityNeverCarriesAPassword(t *testing.T) {
	for _, user := range []string{"", "actor"} {
		rec := dummyRecord()
		rec.ActionUsername = user
		m := dbRecordToMap(rec)
		ident, ok := m["action_identity"].(map[string]any)
		if !ok {
			t.Fatalf("action_identity = %#v, want an object", m["action_identity"])
		}
		if ident["configured"] != (user != "") || ident["username"] != user {
			t.Fatalf("action_identity = %v for user %q", ident, user)
		}
		raw, _ := json.Marshal(m)
		if strings.Contains(string(raw), "action_password") {
			t.Fatalf("record map names an action password: %s", raw)
		}
	}
}

func TestWriteDatabaseInputError_Codes(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		handled  bool
		status   int
		wantCode string
	}{
		{"action identity", fmt.Errorf("%w: %w: role reused", store.ErrValidation,
			store.ErrActionIdentity), true, 400, "invalid_action_identity"},
		{"other validation", fmt.Errorf("%w: name is required", store.ErrValidation),
			true, 400, ""},
		{"internal", errors.New("connection refused"), false, 0, ""},
		{"nil", nil, false, 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			if got := writeDatabaseInputError(w, tc.err); got != tc.handled {
				t.Fatalf("handled = %t, want %t", got, tc.handled)
			}
			if !tc.handled {
				if w.Body.Len() != 0 {
					t.Fatalf("unhandled error wrote %q", w.Body.String())
				}
				return
			}
			var body map[string]string
			_ = json.Unmarshal(w.Body.Bytes(), &body)
			if w.Code != tc.status || body["code"] != tc.wantCode || body["error"] == "" {
				t.Fatalf("status %d body %v; want %d code %q", w.Code, body, tc.status,
					tc.wantCode)
			}
		})
	}
}

func TestActionTestConnString_SwapsOnlyTheRole(t *testing.T) {
	obs := "postgres://observer:obs-pw@db.example.com:6543/app?sslmode=require&connect_timeout=10"
	got, err := actionTestConnString(obs, "actor", "act-pw")
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(got)
	pw, _ := u.User.Password()
	if u.User.Username() != "actor" || pw != "act-pw" || u.Host != "db.example.com:6543" ||
		u.Path != "/app" || u.Query().Get("sslmode") != "require" {
		t.Fatalf("action test DSN = %s", u.Redacted())
	}
	if err := config.ValidateActionDSN(obs, got); err != nil {
		t.Fatalf("derived DSN is not another role on the same database: %v", err)
	}
	for _, bad := range []string{"", "host=db user=x", "mysql://x@y/z"} {
		if _, err := actionTestConnString(bad, "actor", "pw"); err == nil {
			t.Errorf("actionTestConnString(%q) succeeded", bad)
		}
	}
	if _, err := actionTestConnString(obs, "", "pw"); err == nil {
		t.Error("an empty action role was accepted")
	}
}

func TestCSVImport_RefusesActionColumns(t *testing.T) {
	csvBody := "name,host,port,database_name,username,password,sslmode,action_username," +
		"action_password\nprod,db.example.com,5432,app,sage,pw,require,actor,act-pw\n"
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "dbs.csv")
	_, _ = fw.Write([]byte(csvBody))
	_ = mw.Close()
	req := httptest.NewRequest("POST", "/api/v1/databases/managed/import", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	importCSVHandler(&DatabaseDeps{})(w, req)
	var res csvImportResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v: %s", err, w.Body.String())
	}
	if res.Imported != 0 || len(res.Errors) != 1 ||
		!strings.Contains(res.Errors[0].Error, "through the API") {
		t.Fatalf("import result = %+v, want the action columns refused", res)
	}
	if strings.Contains(w.Body.String(), "act-pw") {
		t.Fatal("import response echoes the action password")
	}
}

// Integration: the API writes, shows and removes the credential and never
// echoes the password; refusals are 400 invalid_action_identity.
func TestActionCredentialAPI_CreateShowUpdateRemove(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	if _, err := pool.Exec(ctx, "DELETE FROM sage.databases"); err != nil {
		t.Fatal(err)
	}
	deps := &DatabaseDeps{Store: store.NewDatabaseStore(pool, p2Key)}
	base := `"name":"m","host":"db.example.com","port":5432,"database_name":"app",` +
		`"username":"observer","password":"obs-pw","sslmode":"require",` +
		`"trust_level":"observation","execution_mode":"approval"`
	refusals := []string{
		`{` + base + `,"action_username":"observer","action_password":"secret-act-1"}`,
		`{` + base + `,"action_password":"secret-act-1"}`,
		`{` + base + `,"action_username":"actor"}`,
	}
	for _, body := range refusals {
		w := callDB(t, createManagedDBHandler(deps), "POST", "", body)
		assertActionRefusal(t, w)
	}
	w := callDB(t, createManagedDBHandler(deps), "POST", "",
		`{`+base+`,"action_username":"actor","action_password":"secret-act-1"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", w.Code, w.Body.String())
	}
	created := decodeRecord(t, w)
	id := strconv.Itoa(int(created["id"].(float64)))
	assertIdentity(t, created, true, "actor")
	got := decodeRecord(t, callDB(t, getManagedDBHandler(deps), "GET", id, ""))
	assertIdentity(t, got, true, "actor")
	w = callDB(t, updateManagedDBHandler(deps), "PUT", id,
		`{`+base+`,"action_username":"observer"}`)
	assertActionRefusal(t, w)
	w = callDB(t, updateManagedDBHandler(deps), "PUT", id, `{`+base+`}`)
	assertIdentity(t, decodeRecord(t, w), true, "actor")
	w = callDB(t, updateManagedDBHandler(deps), "PUT", id, `{`+base+`,"action_username":""}`)
	assertIdentity(t, decodeRecord(t, w), false, "")
}

func callDB(t *testing.T, h http.HandlerFunc, method, id, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/x", strings.NewReader(body))
	if id != "" {
		req.SetPathValue("id", id)
	}
	w := httptest.NewRecorder()
	h(w, withUser(req, testAdminUser()))
	if strings.Contains(w.Body.String(), "secret-act-1") {
		t.Fatalf("response echoes the action password: %s", w.Body.String())
	}
	return w
}

func assertActionRefusal(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	var body map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if w.Code != http.StatusBadRequest || body["code"] != "invalid_action_identity" {
		t.Fatalf("status %d body %s; want 400 invalid_action_identity", w.Code,
			w.Body.String())
	}
}

func decodeRecord(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode %d %s: %v", w.Code, w.Body.String(), err)
	}
	return m
}

func assertIdentity(t *testing.T, m map[string]any, configured bool, user string) {
	t.Helper()
	ident, _ := m["action_identity"].(map[string]any)
	if ident["configured"] != configured || ident["username"] != user {
		t.Fatalf("action_identity = %v, want configured=%t username=%q", ident,
			configured, user)
	}
}
