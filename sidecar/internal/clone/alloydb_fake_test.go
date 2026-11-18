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

// alloyDBFake is a stateful stand-in for the AlloyDB Admin API v1
// (lab-05f): one primary cluster "shop" in acme-prod-1234/us-central1 with
// continuous backup on, a CMEK key, a primary instance on a private and a
// public IP, and a read pool. A restored cluster or a new instance is
// CREATING and its operation running for doneAfter polls, then READY.
// Set the knobs before the provider is used.
type alloyDBFake struct {
	mu        sync.Mutex
	clusters  map[string]*fakeADBCluster
	instances map[string]*fakeADBInstance // "<cluster>/<instance>"
	ops       map[string]*fakeADBOp
	calls     []string
	bodies    map[string]map[string]any
	seq       int

	doneAfter       int
	restoreNever    bool   // the restore operation never finishes
	instanceNever   bool   // the instance operation never finishes
	restoreOpError  string // the restore operation ends with this error
	instanceOpError string // the instance operation ends with this error
	restoreStatus   int    // the restore is refused with this status; nothing is made
	restoreThenCode int    // the restore is made, then this status is returned
	instanceStatus  int    // the instance create is refused with this status
	deleteStatus    int
	getStatus       int             // every cluster read fails with this status
	forbid          map[string]bool // "get", "list", "restore", "instance", "delete", "op"
	cloneIP         string          // the new instance's private IP ("": a fresh one)
	dropLabels      bool            // the restore ignores the requested labels
	restoreOpName   string          // the restore reply names this operation
}

type fakeADBCluster struct {
	Name, State, Type, Version string
	Backup                     bool
	Earliest                   time.Time
	Labels                     map[string]string
}

type fakeADBInstance struct {
	Cluster, Name, State, Type, Private, Public string
	CPU                                         float64
	PublicOn                                    bool
}

type fakeADBOp struct {
	id, cluster, instance, errMsg string
	never                         bool
	polls                         int
}

const (
	adbProject  = "acme-prod-1234"
	adbLocation = "us-central1"
	adbParent   = "projects/" + adbProject + "/locations/" + adbLocation
	adbSource   = "shop"
	adbProdIP   = "10.0.0.5"
	adbPublicIP = "34.1.2.3"
	adbPassword = "adb-s3cret"
	adbProdDSN  = "postgres://app:" + adbPassword + "@" + adbProdIP +
		":5432/shop?sslmode=prefer"
	adbName  = "sage-rehearsal-0a1b2c3d"
	adbToken = "ya29.fake-alloydb-token"
	adbKMS   = "projects/acme-prod-1234/locations/us-central1/keyRings/db/cryptoKeys/alloydb"
	adbVPC   = "projects/acme-prod-1234/global/networks/prod-vpc"
)

var adbNow = time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)

func newAlloyDBFake(t *testing.T) (*alloyDBFake, string) {
	t.Helper()
	f := &alloyDBFake{clusters: map[string]*fakeADBCluster{},
		instances: map[string]*fakeADBInstance{}, ops: map[string]*fakeADBOp{},
		bodies: map[string]map[string]any{}, forbid: map[string]bool{}, doneAfter: 1}
	f.clusters[adbSource] = &fakeADBCluster{Name: adbSource, State: "READY",
		Type: "PRIMARY", Version: "POSTGRES_16", Backup: true,
		Earliest: adbNow.Add(-7 * 24 * time.Hour), Labels: map[string]string{"team": "shop"}}
	f.instances[adbSource+"/shop-primary"] = &fakeADBInstance{Cluster: adbSource,
		Name: "shop-primary", State: "READY", Type: "PRIMARY", Private: adbProdIP,
		Public: adbPublicIP, CPU: 4, PublicOn: true}
	f.instances[adbSource+"/shop-read"] = &fakeADBInstance{Cluster: adbSource,
		Name: "shop-read", State: "READY", Type: "READ_POOL", Private: "10.0.0.6", CPU: 2}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv.URL
}

func (f *alloyDBFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+adbToken {
		writeSQLError(w, http.StatusUnauthorized, "missing token")
		return
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/v1/"+adbParent+"/")
	if !ok {
		writeSQLError(w, http.StatusNotFound, "unknown project or location")
		return
	}
	var body map[string]any
	if raw, _ := io.ReadAll(r.Body); len(raw) > 0 {
		_ = json.Unmarshal(raw, &body)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.route(w, r, strings.Split(rest, "/"), body)
}

func (f *alloyDBFake) route(w http.ResponseWriter, r *http.Request, p []string,
	body map[string]any) {
	q := r.URL.Query()
	switch {
	case len(p) == 2 && p[0] == "operations" && r.Method == http.MethodGet:
		f.operation(w, p[1])
	case len(p) == 1 && p[0] == "clusters:restore" && r.Method == http.MethodPost:
		f.restore(w, body)
	case len(p) == 2 && p[0] == "clusters" && r.Method == http.MethodGet:
		f.getCluster(w, p[1])
	case len(p) == 2 && p[0] == "clusters" && r.Method == http.MethodDelete:
		f.deleteCluster(w, p[1], q.Get("force"))
	case len(p) == 3 && p[2] == "instances" && r.Method == http.MethodGet:
		f.listInstances(w, p[1])
	case len(p) == 3 && p[2] == "instances" && r.Method == http.MethodPost:
		f.createInstance(w, p[1], q.Get("instanceId"), body)
	case len(p) == 4 && p[2] == "instances" && r.Method == http.MethodGet:
		f.getInstance(w, p[1], p[3])
	default:
		writeSQLError(w, http.StatusNotFound, "no route "+r.Method+" "+r.URL.Path)
	}
}

func (f *alloyDBFake) newOp(cluster, instance, errMsg string, never bool) *fakeADBOp {
	f.seq++
	op := &fakeADBOp{id: fmt.Sprintf("operation-%d", f.seq), cluster: cluster,
		instance: instance, errMsg: errMsg, never: never}
	f.ops[op.id] = op
	return op
}

func (op *fakeADBOp) json(done bool) map[string]any {
	target := adbParent + "/clusters/" + op.cluster
	if op.instance != "" {
		target += "/instances/" + op.instance
	}
	out := map[string]any{"name": adbParent + "/operations/" + op.id, "done": done,
		"metadata": map[string]any{"target": target, "verb": "create"}}
	if done && op.errMsg != "" {
		out["error"] = map[string]any{"code": 13, "message": op.errMsg}
	}
	return out
}

func (f *alloyDBFake) operation(w http.ResponseWriter, id string) {
	if f.forbid["op"] {
		writeSQLError(w, http.StatusForbidden, "not authorized")
		return
	}
	op, ok := f.ops[id]
	if !ok {
		writeSQLError(w, http.StatusNotFound, "no operation "+id)
		return
	}
	op.polls++
	done := !op.never && op.polls >= f.doneAfter
	if done {
		f.finish(op)
	}
	writeSQLJSON(w, op.json(done))
}

// finish moves the operation's resource out of CREATING.
func (f *alloyDBFake) finish(op *fakeADBOp) {
	state := "READY"
	if op.errMsg != "" {
		state = "FAILED"
	}
	if op.instance != "" {
		if in := f.instances[op.cluster+"/"+op.instance]; in != nil && in.State == "CREATING" {
			in.State = state
		}
		return
	}
	if c := f.clusters[op.cluster]; c != nil && c.State == "CREATING" {
		c.State = state
	}
}

func (f *alloyDBFake) restore(w http.ResponseWriter, body map[string]any) {
	id, _ := body["clusterId"].(string)
	f.calls = append(f.calls, "restore "+id)
	f.bodies["restore "+id] = body
	switch {
	case f.forbid["restore"]:
		writeSQLError(w, http.StatusForbidden, "Permission 'alloydb.clusters.restore' denied")
		return
	case f.restoreStatus != 0:
		writeSQLError(w, f.restoreStatus, "restore refused")
		return
	case f.clusters[id] != nil:
		writeSQLError(w, http.StatusConflict, "Resource already exists")
		return
	}
	c := &fakeADBCluster{Name: id, State: "CREATING", Type: "PRIMARY",
		Version: "POSTGRES_16", Labels: map[string]string{}}
	cluster, _ := body["cluster"].(map[string]any)
	if labels, ok := cluster["labels"].(map[string]any); ok && !f.dropLabels {
		for k, v := range labels {
			c.Labels[k], _ = v.(string)
		}
	}
	f.clusters[id] = c
	op := f.newOp(id, "", f.restoreOpError, f.restoreNever)
	if f.restoreThenCode != 0 {
		writeSQLError(w, f.restoreThenCode, "backend error")
		return
	}
	reply := op.json(false)
	if f.restoreOpName != "" {
		reply["name"] = f.restoreOpName
	}
	writeSQLJSON(w, reply)
}

func (f *alloyDBFake) getCluster(w http.ResponseWriter, name string) {
	f.calls = append(f.calls, "get cluster "+name)
	switch {
	case f.forbid["get"]:
		writeSQLError(w, http.StatusForbidden, "not authorized")
		return
	case f.getStatus != 0:
		writeSQLError(w, f.getStatus, "read failed")
		return
	}
	c, ok := f.clusters[name]
	if !ok {
		writeSQLError(w, http.StatusNotFound, "Resource not found")
		return
	}
	writeSQLJSON(w, c.json())
}

func (c *fakeADBCluster) json() map[string]any {
	out := map[string]any{"name": adbParent + "/clusters/" + c.Name, "state": c.State,
		"clusterType": c.Type, "databaseVersion": c.Version, "labels": c.Labels,
		"networkConfig": map[string]any{"network": adbVPC,
			"allocatedIpRange": "adb-range"},
		"encryptionConfig": map[string]any{"kmsKeyName": adbKMS},
		"continuousBackupConfig": map[string]any{"enabled": c.Backup,
			"recoveryWindowDays": 14}}
	if c.Backup {
		out["continuousBackupInfo"] = map[string]any{
			"earliestRestorableTime": c.Earliest.Format(time.RFC3339Nano)}
	}
	return out
}

func (f *alloyDBFake) listInstances(w http.ResponseWriter, cluster string) {
	f.calls = append(f.calls, "list instances "+cluster)
	if f.forbid["list"] {
		writeSQLError(w, http.StatusForbidden, "not authorized")
		return
	}
	var list []map[string]any
	for _, in := range f.instances {
		if in.Cluster == cluster {
			list = append(list, in.json())
		}
	}
	writeSQLJSON(w, map[string]any{"instances": list})
}

func (in *fakeADBInstance) json() map[string]any {
	out := map[string]any{"name": adbParent + "/clusters/" + in.Cluster + "/instances/" +
		in.Name, "state": in.State, "instanceType": in.Type, "ipAddress": in.Private,
		"machineConfig": map[string]any{"cpuCount": in.CPU},
		"networkConfig": map[string]any{"enablePublicIp": in.PublicOn,
			"authorizedExternalNetworks": []map[string]any{{"cidrRange": "203.0.113.0/24"}}}}
	if in.PublicOn {
		out["publicIpAddress"] = in.Public
	}
	return out
}

func (f *alloyDBFake) createInstance(w http.ResponseWriter, cluster, id string,
	body map[string]any) {
	f.calls = append(f.calls, "create instance "+cluster+"/"+id)
	f.bodies["instance "+cluster+"/"+id] = body
	switch {
	case f.forbid["instance"]:
		writeSQLError(w, http.StatusForbidden, "Permission 'alloydb.instances.create' denied")
		return
	case f.instanceStatus != 0:
		writeSQLError(w, f.instanceStatus, "instance refused")
		return
	case f.clusters[cluster] == nil:
		writeSQLError(w, http.StatusNotFound, "Resource not found")
		return
	}
	n := len(f.instances) + 10
	in := &fakeADBInstance{Cluster: cluster, Name: id, State: "CREATING", Type: "PRIMARY",
		Private: fmt.Sprintf("10.0.0.%d", n), Public: fmt.Sprintf("34.1.2.%d", n)}
	if f.cloneIP != "" {
		in.Private = f.cloneIP
	}
	if nc, ok := body["networkConfig"].(map[string]any); ok {
		in.PublicOn, _ = nc["enablePublicIp"].(bool)
	}
	if mc, ok := body["machineConfig"].(map[string]any); ok {
		in.CPU, _ = mc["cpuCount"].(float64)
	}
	f.instances[cluster+"/"+id] = in
	writeSQLJSON(w, f.newOp(cluster, id, f.instanceOpError, f.instanceNever).json(false))
}

func (f *alloyDBFake) getInstance(w http.ResponseWriter, cluster, id string) {
	f.calls = append(f.calls, "get instance "+cluster+"/"+id)
	in, ok := f.instances[cluster+"/"+id]
	if !ok {
		writeSQLError(w, http.StatusNotFound, "Resource not found")
		return
	}
	writeSQLJSON(w, in.json())
}

func (f *alloyDBFake) deleteCluster(w http.ResponseWriter, name, force string) {
	f.calls = append(f.calls, "delete "+name+" force="+force)
	switch {
	case f.forbid["delete"]:
		writeSQLError(w, http.StatusForbidden, "not authorized")
		return
	case f.deleteStatus != 0:
		writeSQLError(w, f.deleteStatus, "delete failed")
		return
	}
	if _, ok := f.clusters[name]; !ok {
		writeSQLError(w, http.StatusNotFound, "Resource not found")
		return
	}
	for key, in := range f.instances {
		if in.Cluster == name {
			if force != "true" {
				writeSQLError(w, http.StatusBadRequest, "cluster has instances")
				return
			}
			delete(f.instances, key)
		}
	}
	delete(f.clusters, name)
	writeSQLJSON(w, f.newOp(name, "", "", false).json(true))
}

func (f *alloyDBFake) exists(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.clusters[name]
	return ok
}

func (f *alloyDBFake) cluster(name string) fakeADBCluster {
	f.mu.Lock()
	defer f.mu.Unlock()
	return *f.clusters[name]
}

func (f *alloyDBFake) instance(cluster, name string) (fakeADBInstance, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	in, ok := f.instances[cluster+"/"+name]
	if !ok {
		return fakeADBInstance{}, false
	}
	return *in, true
}

// put adds a cluster as AlloyDB would describe it.
func (f *alloyDBFake) put(c fakeADBCluster) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clusters[c.Name] = &c
}

// mutating is every restore, instance create and delete, in order.
func (f *alloyDBFake) mutating() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if !strings.HasPrefix(c, "get ") && !strings.HasPrefix(c, "list ") {
			out = append(out, c)
		}
	}
	return out
}

func (f *alloyDBFake) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *alloyDBFake) body(key string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bodies[key]
}

func adbConfig(base string) clone.AlloyDBRestoreConfig {
	return clone.AlloyDBRestoreConfig{Project: adbProject, Location: adbLocation,
		Cluster: adbSource, Deadline: 2 * time.Hour, TTL: 3*time.Hour + 15*time.Minute,
		BaseURL: base, PollEvery: time.Millisecond, Now: func() time.Time { return adbNow },
		Token: func(context.Context) (clone.CloudSQLTokenSource, error) {
			return staticToken{tok: adbToken}, nil
		},
		Production: func(context.Context) (clone.AlloyDBProduction, error) {
			return clone.AlloyDBProduction{DSN: adbProdDSN}, nil
		}}
}

func newAlloyDBAPI(t *testing.T, base string,
	edit func(*clone.AlloyDBRestoreConfig)) *clone.AlloyDBRestoreAPI {
	t.Helper()
	cfg := adbConfig(base)
	if edit != nil {
		edit(&cfg)
	}
	api, err := clone.NewAlloyDBRestoreAPI(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return api
}

func newAlloyDBProvider(t *testing.T, base string,
	edit func(*clone.AlloyDBRestoreConfig)) *clone.SnapshotProvider {
	t.Helper()
	p, err := clone.NewSnapshotProvider(newAlloyDBAPI(t, base, edit),
		func() time.Time { return adbNow })
	if err != nil {
		t.Fatal(err)
	}
	return p
}
