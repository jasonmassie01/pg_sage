package clone_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/clone"
)

// cloudSQLFake is a stateful stand-in for the Cloud SQL Admin API v1
// (lab-05e): one Cloud SQL for PostgreSQL source instance "shop" in
// project acme-prod-1234 with a public and a private IP, deletion
// protection on and regional HA. A clone is PENDING_CREATE and its
// operation RUNNING for doneAfter operation polls, then RUNNABLE and DONE.
// The knobs inject the failures the provider must handle; set them before
// the provider is used.
type cloudSQLFake struct {
	mu        sync.Mutex
	instances map[string]*fakeSQLInstance
	ops       map[string]*fakeSQLOp
	calls     []string
	bodies    map[string]map[string]any
	seq       int

	doneAfter     int
	neverDone     bool
	opError       string // a clone operation ends DONE with this error, the instance FAILED
	cloneStatus   int    // the clone is refused with this status; nothing is made
	cloneThenCode int    // the clone is made, then this status is returned
	patchStatus   int
	deleteStatus  int
	getStatus     int             // every instance read fails with this status
	forbid        map[string]bool // op ("get", "clone", "patch", "delete", "operation"): 403
	cloneIP       string          // the clone's private IP ("": a fresh one)
}

type fakeSQLInstance struct {
	Name, State, Version, Type, Tier, Availability string
	Primary, Private                               string
	Labels                                         map[string]string
	Protected                                      bool
}

type fakeSQLOp struct {
	name, target, kind, errMsg string
	polls                      int
}

const (
	sqlProject  = "acme-prod-1234"
	sqlSource   = "shop"
	sqlProdIP   = "10.0.0.5"
	sqlPassword = "sql-s3cret"
	sqlProdDSN  = "postgres://app:" + sqlPassword + "@" + sqlProdIP +
		":5432/shop?sslmode=prefer"
	sqlName  = "sage-rehearsal-0a1b2c3d"
	sqlToken = "ya29.fake-token"
)

var sqlNow = time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)

func newCloudSQLFake(t *testing.T) (*cloudSQLFake, string) {
	t.Helper()
	f := &cloudSQLFake{instances: map[string]*fakeSQLInstance{}, ops: map[string]*fakeSQLOp{},
		bodies: map[string]map[string]any{}, forbid: map[string]bool{}, doneAfter: 1}
	f.instances[sqlSource] = &fakeSQLInstance{Name: sqlSource, State: "RUNNABLE",
		Version: "POSTGRES_16", Type: "CLOUD_SQL_INSTANCE", Tier: "db-custom-4-16384",
		Availability: "REGIONAL", Primary: "34.1.2.3", Private: sqlProdIP,
		Labels: map[string]string{"team": "shop"}, Protected: true}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv.URL
}

func (f *cloudSQLFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+sqlToken {
		writeSQLError(w, http.StatusUnauthorized, "missing token")
		return
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/v1/projects/"+sqlProject+"/")
	if !ok {
		writeSQLError(w, http.StatusNotFound, "unknown project")
		return
	}
	var body map[string]any
	if raw, _ := io.ReadAll(r.Body); len(raw) > 0 {
		_ = json.Unmarshal(raw, &body)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	parts := strings.Split(rest, "/")
	switch {
	case len(parts) == 2 && parts[0] == "operations" && r.Method == http.MethodGet:
		f.operation(w, parts[1])
	case len(parts) == 3 && parts[2] == "clone" && r.Method == http.MethodPost:
		f.clone(w, parts[1], body)
	case len(parts) == 2 && parts[0] == "instances" && r.Method == http.MethodGet:
		f.get(w, parts[1])
	case len(parts) == 2 && parts[0] == "instances" && r.Method == http.MethodPatch:
		f.patch(w, parts[1], body)
	case len(parts) == 2 && parts[0] == "instances" && r.Method == http.MethodDelete:
		f.delete(w, parts[1])
	default:
		writeSQLError(w, http.StatusNotFound, "no route "+r.Method+" "+rest)
	}
}

func writeSQLError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": code,
		"message": msg}})
}

func writeSQLJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (f *cloudSQLFake) newOp(kind, target, errMsg string) *fakeSQLOp {
	f.seq++
	op := &fakeSQLOp{name: fmt.Sprintf("op-%s-%d", kind, f.seq), target: target,
		kind: kind, errMsg: errMsg}
	f.ops[op.name] = op
	return op
}

func (op *fakeSQLOp) json(done bool) map[string]any {
	out := map[string]any{"name": op.name, "targetId": op.target,
		"operationType": strings.ToUpper(op.kind), "status": "RUNNING"}
	if done {
		out["status"] = "DONE"
		if op.errMsg != "" {
			out["error"] = map[string]any{"errors": []map[string]any{{
				"code": "INTERNAL_ERROR", "message": op.errMsg}}}
		}
	}
	return out
}

func (f *cloudSQLFake) operation(w http.ResponseWriter, name string) {
	if f.forbid["operation"] {
		writeSQLError(w, http.StatusForbidden, "not authorized")
		return
	}
	op, ok := f.ops[name]
	if !ok {
		writeSQLError(w, http.StatusNotFound, "no operation "+name)
		return
	}
	op.polls++
	done := op.kind != "clone" || (!f.neverDone && op.polls >= f.doneAfter)
	if done && op.kind == "clone" {
		if in := f.instances[op.target]; in != nil && in.State == "PENDING_CREATE" {
			in.State = "RUNNABLE"
			if op.errMsg != "" {
				in.State = "FAILED"
			}
		}
	}
	writeSQLJSON(w, op.json(done))
}

func (f *cloudSQLFake) clone(w http.ResponseWriter, source string, body map[string]any) {
	ctx, _ := body["cloneContext"].(map[string]any)
	dest, _ := ctx["destinationInstanceName"].(string)
	f.calls = append(f.calls, "clone "+source+" to "+dest)
	f.bodies["clone "+dest] = body
	switch {
	case f.forbid["clone"]:
		writeSQLError(w, http.StatusForbidden, "The client is not authorized to make "+
			"this request.")
		return
	case f.cloneStatus != 0:
		writeSQLError(w, f.cloneStatus, "clone refused")
		return
	case f.instances[dest] != nil:
		writeSQLError(w, http.StatusConflict, "The Cloud SQL instance already exists.")
		return
	}
	src, ok := f.instances[source]
	if !ok {
		writeSQLError(w, http.StatusNotFound, "The Cloud SQL instance does not exist.")
		return
	}
	n := len(f.instances) + 10
	in := *src
	in.Name, in.State, in.Labels = dest, "PENDING_CREATE", map[string]string{}
	in.Primary, in.Private = fmt.Sprintf("34.1.2.%d", n), fmt.Sprintf("10.0.0.%d", n)
	for k, v := range src.Labels {
		in.Labels[k] = v
	}
	if f.cloneIP != "" {
		in.Private = f.cloneIP
	}
	f.instances[dest] = &in
	op := f.newOp("clone", dest, f.opError)
	if f.cloneThenCode != 0 {
		writeSQLError(w, f.cloneThenCode, "backend error")
		return
	}
	writeSQLJSON(w, op.json(false))
}

func (f *cloudSQLFake) get(w http.ResponseWriter, name string) {
	f.calls = append(f.calls, "get "+name)
	if f.forbid["get"] {
		writeSQLError(w, http.StatusForbidden, "not authorized")
		return
	}
	if f.getStatus != 0 {
		writeSQLError(w, f.getStatus, "read failed")
		return
	}
	in, ok := f.instances[name]
	if !ok {
		writeSQLError(w, http.StatusNotFound, "The Cloud SQL instance does not exist.")
		return
	}
	writeSQLJSON(w, in.json())
}

func (in *fakeSQLInstance) json() map[string]any {
	return map[string]any{"name": in.Name, "project": sqlProject, "state": in.State,
		"databaseVersion": in.Version, "instanceType": in.Type, "region": "us-central1",
		"ipAddresses": []map[string]string{{"type": "PRIMARY", "ipAddress": in.Primary},
			{"type": "PRIVATE", "ipAddress": in.Private}},
		"settings": map[string]any{"tier": in.Tier, "userLabels": in.Labels,
			"deletionProtectionEnabled": in.Protected, "availabilityType": in.Availability}}
}

func (f *cloudSQLFake) patch(w http.ResponseWriter, name string, body map[string]any) {
	f.calls = append(f.calls, "patch "+name)
	f.bodies["patch "+name] = body
	if f.forbid["patch"] {
		writeSQLError(w, http.StatusForbidden, "not authorized")
		return
	}
	if f.patchStatus != 0 {
		writeSQLError(w, f.patchStatus, "patch failed")
		return
	}
	in, ok := f.instances[name]
	if !ok {
		writeSQLError(w, http.StatusNotFound, "The Cloud SQL instance does not exist.")
		return
	}
	settings, _ := body["settings"].(map[string]any)
	if labels, ok := settings["userLabels"].(map[string]any); ok {
		in.Labels = map[string]string{}
		for k, v := range labels {
			in.Labels[k], _ = v.(string)
		}
	}
	if p, ok := settings["deletionProtectionEnabled"].(bool); ok {
		in.Protected = p
	}
	if a, ok := settings["availabilityType"].(string); ok {
		in.Availability = a
	}
	writeSQLJSON(w, f.newOp("update", name, "").json(false))
}

func (f *cloudSQLFake) delete(w http.ResponseWriter, name string) {
	f.calls = append(f.calls, "delete "+name)
	if f.forbid["delete"] {
		writeSQLError(w, http.StatusForbidden, "not authorized")
		return
	}
	if f.deleteStatus != 0 {
		writeSQLError(w, f.deleteStatus, "delete failed")
		return
	}
	in, ok := f.instances[name]
	if !ok {
		writeSQLError(w, http.StatusNotFound, "The Cloud SQL instance does not exist.")
		return
	}
	if in.Protected {
		writeSQLError(w, http.StatusBadRequest, "deletion protection is enabled")
		return
	}
	delete(f.instances, name)
	writeSQLJSON(w, f.newOp("delete", name, "").json(false))
}

func (f *cloudSQLFake) exists(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.instances[name]
	return ok
}

func (f *cloudSQLFake) instance(name string) fakeSQLInstance {
	f.mu.Lock()
	defer f.mu.Unlock()
	return *f.instances[name]
}

// mutating is every clone, patch and delete call, in order.
func (f *cloudSQLFake) mutating() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if !strings.HasPrefix(c, "get ") {
			out = append(out, c)
		}
	}
	return out
}

// put adds an instance as Cloud SQL would describe it.
func (f *cloudSQLFake) put(in fakeSQLInstance) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.instances[in.Name] = &in
}

func (f *cloudSQLFake) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *cloudSQLFake) body(key string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bodies[key]
}

type staticToken struct {
	tok string
	err error
}

func (s staticToken) Token(context.Context) (string, error) { return s.tok, s.err }

func sqlConfig(base string) clone.CloudSQLCloneConfig {
	return clone.CloudSQLCloneConfig{Project: sqlProject, Instance: sqlSource,
		Deadline: 2 * time.Hour, TTL: 3*time.Hour + 15*time.Minute, BaseURL: base,
		PollEvery: time.Millisecond, Now: func() time.Time { return sqlNow },
		Token: func(context.Context) (clone.CloudSQLTokenSource, error) {
			return staticToken{tok: sqlToken}, nil
		},
		Production: func(context.Context) (clone.CloudSQLProduction, error) {
			return clone.CloudSQLProduction{DSN: sqlProdDSN}, nil
		}}
}

func newCloudSQLAPI(t *testing.T, base string,
	edit func(*clone.CloudSQLCloneConfig)) *clone.CloudSQLCloneAPI {
	t.Helper()
	cfg := sqlConfig(base)
	if edit != nil {
		edit(&cfg)
	}
	api, err := clone.NewCloudSQLCloneAPI(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return api
}

func newCloudSQLProvider(t *testing.T, base string,
	edit func(*clone.CloudSQLCloneConfig)) *clone.SnapshotProvider {
	t.Helper()
	p, err := clone.NewSnapshotProvider(newCloudSQLAPI(t, base, edit),
		func() time.Time { return sqlNow })
	if err != nil {
		t.Fatal(err)
	}
	return p
}
