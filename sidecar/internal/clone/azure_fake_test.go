package clone_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/clone"
)

// azureFake is a stateful stand-in for Azure Resource Manager's
// Microsoft.DBforPostgreSQL/flexibleServers (lab-05g): a primary server
// "shop" in eastus reached on its FQDN, in a delegated subnet, encrypted
// with a customer key under a user-assigned identity, zone-redundant, and
// its read replica. A PUT makes a restored server Provisioning with an
// async operation that succeeds after doneAfter polls. Set the knobs
// before the provider is used.
type azureFake struct {
	mu      sync.Mutex
	servers map[string]*fakeAZServer
	ops     map[string]*fakeAZOp
	calls   []string
	bodies  map[string]map[string]any
	headers map[string]http.Header
	seq     int
	base    string

	doneAfter     int
	putNever      bool            // the restore's operation never finishes
	putOpError    string          // the restore's operation fails with this message
	putStatus     int             // the PUT is refused with this status; nothing is made
	putThenCode   int             // the server is made, then this status is returned
	deleteStatus  int             // every DELETE fails with this status
	getStatus     int             // every server read fails with this status
	forbid        map[string]bool // "read", "write", "delete", "op"
	takenGlobally map[string]bool // names another subscription holds: PUT is 409
	dropTags      bool            // the restore ignores the requested tags
	asyncURL      string          // the PUT reply names this operation URL
	locationOnly  bool            // the PUT reply has a Location header only
	finalState    string          // the restored server's state when done ("": Ready)
	cloneFQDN     string          // the restored server's FQDN ("": <name>.<suffix>)
	opPollAuth    []string        // the Authorization header of every operation poll
	asyncHits     int             // requests for the path asyncURL names
}

type fakeAZServer struct {
	Name, State, Role, FQDN, Version string
	Earliest                         time.Time
	Tags                             map[string]string
	Body                             map[string]any // the PUT that made it
	Public                           bool           // public access, no delegated subnet
	SystemKey                        bool           // service-managed encryption
}

type fakeAZOp struct {
	id, server, errMsg string
	never              bool
	polls              int
}

const (
	azSub       = "0f1e2d3c-4b5a-4697-8877-665544332211"
	azGroup     = "rg-shop-prod"
	azServer    = "shop"
	azLocation  = "eastus"
	azSuffix    = ".postgres.database.azure.com"
	azProdFQDN  = azServer + azSuffix
	azPassword  = "az-s3cret"
	azProdDSN   = "postgres://app:" + azPassword + "@" + azProdFQDN + ":5432/shop?sslmode=prefer"
	azName      = "sage-rehearsal-0a1b2c3d"
	azToken     = "eyJ.fake-arm-token"
	azServersID = "/subscriptions/" + azSub + "/resourceGroups/" + azGroup +
		"/providers/Microsoft.DBforPostgreSQL/flexibleServers/"
	azSourceID = azServersID + azServer
	azOpPrefix = "/subscriptions/" + azSub +
		"/providers/Microsoft.DBforPostgreSQL/locations/" + azLocation + "/azureAsyncOperation/"
	azSubnet = "/subscriptions/" + azSub + "/resourceGroups/" + azGroup +
		"/providers/Microsoft.Network/virtualNetworks/prod/subnets/postgres"
	azDNSZone = "/subscriptions/" + azSub + "/resourceGroups/" + azGroup +
		"/providers/Microsoft.Network/privateDnsZones/shop.private.postgres.database.azure.com"
	azIdentity = "/subscriptions/" + azSub + "/resourceGroups/" + azGroup +
		"/providers/Microsoft.ManagedIdentity/userAssignedIdentities/pg-cmk"
	azKey = "https://shop-kv.vault.azure.net/keys/pg/0123456789abcdef"
)

var azNow = time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)

func newAzureFake(t *testing.T) (*azureFake, string) {
	t.Helper()
	f := &azureFake{servers: map[string]*fakeAZServer{}, ops: map[string]*fakeAZOp{},
		bodies: map[string]map[string]any{}, headers: map[string]http.Header{},
		forbid: map[string]bool{}, takenGlobally: map[string]bool{}, doneAfter: 1}
	f.servers[azServer] = &fakeAZServer{Name: azServer, State: "Ready", Role: "Primary",
		FQDN: azProdFQDN, Version: "16", Earliest: azNow.Add(-7 * 24 * time.Hour),
		Tags: map[string]string{"team": "shop"}}
	f.servers["shop-replica"] = &fakeAZServer{Name: "shop-replica", State: "Ready",
		Role: "AsyncReplica", FQDN: "shop-replica" + azSuffix, Version: "16",
		Earliest: azNow.Add(-7 * 24 * time.Hour), Tags: map[string]string{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	f.base = srv.URL
	return f, srv.URL
}

func (f *azureFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if raw, _ := io.ReadAll(r.Body); len(raw) > 0 {
		_ = json.Unmarshal(raw, &body)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if strings.HasPrefix(r.URL.Path, azOpPrefix) {
		f.opPollAuth = append(f.opPollAuth, r.Header.Get("Authorization"))
	}
	if u, err := url.Parse(f.asyncURL); f.asyncURL != "" && err == nil && u.Path == r.URL.Path {
		f.asyncHits++
	}
	if r.Header.Get("Authorization") != "Bearer "+azToken {
		writeARMError(w, http.StatusUnauthorized, "InvalidAuthenticationToken", "no token")
		return
	}
	if r.URL.Query().Get("api-version") != clone.AzureFlexibleServerAPIVersion {
		writeARMError(w, http.StatusBadRequest, "MissingApiVersionParameter", "api-version")
		return
	}
	f.route(w, r, body)
}

func (f *azureFake) route(w http.ResponseWriter, r *http.Request, body map[string]any) {
	if id, ok := strings.CutPrefix(r.URL.Path, azOpPrefix); ok && r.Method == http.MethodGet {
		f.operation(w, id)
		return
	}
	name, ok := strings.CutPrefix(r.URL.Path, azServersID)
	if !ok || name == "" || strings.Contains(name, "/") {
		writeARMError(w, http.StatusNotFound, "NotFound", "no route "+r.URL.Path)
		return
	}
	switch r.Method {
	case http.MethodGet:
		f.get(w, name)
	case http.MethodPut:
		f.headers["put "+name] = r.Header.Clone()
		f.put(w, name, body)
	case http.MethodDelete:
		f.delete(w, name)
	default:
		writeARMError(w, http.StatusMethodNotAllowed, "MethodNotAllowed", r.Method)
	}
}

func writeARMError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": code,
		"message": msg}})
}

func (f *azureFake) get(w http.ResponseWriter, name string) {
	f.calls = append(f.calls, "get "+name)
	switch {
	case f.forbid["read"]:
		writeARMError(w, http.StatusForbidden, "AuthorizationFailed", "no read")
		return
	case f.getStatus != 0:
		writeARMError(w, f.getStatus, "InternalServerError", "read failed")
		return
	}
	s, ok := f.servers[name]
	if !ok {
		writeARMError(w, http.StatusNotFound, "ResourceNotFound", "server not found")
		return
	}
	writeSQLJSON(w, s.json())
}

func (s *fakeAZServer) json() map[string]any {
	props := map[string]any{"version": s.Version, "state": s.State,
		"fullyQualifiedDomainName": s.FQDN, "replicationRole": s.Role,
		"administratorLogin": "app",
		"storage":            map[string]any{"storageSizeGB": 512, "autoGrow": "Enabled"},
		"backup": map[string]any{"backupRetentionDays": 14, "geoRedundantBackup": "Enabled",
			"earliestRestoreDate": s.Earliest.Format(time.RFC3339Nano)},
		"network": map[string]any{"publicNetworkAccess": "Disabled",
			"delegatedSubnetResourceId": azSubnet, "privateDnsZoneArmResourceId": azDNSZone},
		"highAvailability": map[string]any{"mode": "ZoneRedundant", "state": "Healthy"},
		"dataEncryption": map[string]any{"type": "AzureKeyVault",
			"primaryKeyURI": azKey, "primaryUserAssignedIdentityId": azIdentity}}
	if s.Public {
		props["network"] = map[string]any{"publicNetworkAccess": "Enabled"}
	}
	if s.SystemKey {
		props["dataEncryption"] = map[string]any{"type": "SystemManaged"}
	}
	out := map[string]any{"id": azServersID + s.Name, "name": s.Name,
		"type": "Microsoft.DBforPostgreSQL/flexibleServers", "location": azLocation,
		"tags": s.Tags, "sku": map[string]any{"name": "Standard_D4ds_v5",
			"tier": "GeneralPurpose"},
		"identity": map[string]any{"type": "UserAssigned",
			"userAssignedIdentities": map[string]any{azIdentity: map[string]any{
				"principalId": "11111111-2222-4333-8444-555555555555"}}},
		"properties": props}
	if s.SystemKey {
		delete(out, "identity")
	}
	return out
}

func (f *azureFake) put(w http.ResponseWriter, name string, body map[string]any) {
	f.calls = append(f.calls, "put "+name)
	f.bodies["put "+name] = body
	switch {
	case f.forbid["write"]:
		writeARMError(w, http.StatusForbidden, "AuthorizationFailed",
			"no Microsoft.DBforPostgreSQL/flexibleServers/write")
		return
	case f.takenGlobally[name]:
		writeARMError(w, http.StatusConflict, "ServerNameAlreadyExists",
			"Specified server name is already used.")
		return
	case f.putStatus != 0:
		writeARMError(w, f.putStatus, "BadRequest", "restore refused")
		return
	case f.servers[name] != nil:
		// ARM's PUT is create-or-update: a real ARM would update it.
		f.servers[name].Body = body
		writeSQLJSON(w, f.servers[name].json())
		return
	}
	fqdn := name + azSuffix
	if f.cloneFQDN != "" {
		fqdn = f.cloneFQDN
	}
	s := &fakeAZServer{Name: name, State: "Provisioning", Role: "Primary", FQDN: fqdn,
		Version: "16", Tags: map[string]string{}, Body: body}
	if tags, ok := body["tags"].(map[string]any); ok && !f.dropTags {
		for k, v := range tags {
			s.Tags[k], _ = v.(string)
		}
	}
	f.servers[name] = s
	f.seq++
	op := &fakeAZOp{id: fmt.Sprintf("op-%d", f.seq), server: name, errMsg: f.putOpError,
		never: f.putNever}
	f.ops[op.id] = op
	if f.putThenCode != 0 {
		writeARMError(w, f.putThenCode, "InternalServerError", "backend error")
		return
	}
	f.writeAccepted(w, op)
}

func (f *azureFake) writeAccepted(w http.ResponseWriter, op *fakeAZOp) {
	opURL := f.base + azOpPrefix + op.id + "?api-version=" + clone.AzureFlexibleServerAPIVersion
	if f.asyncURL != "" {
		opURL = f.asyncURL
	}
	if f.locationOnly {
		w.Header().Set("Location", opURL)
	} else {
		w.Header().Set("Azure-AsyncOperation", opURL)
	}
	w.WriteHeader(http.StatusAccepted)
}

func (f *azureFake) operation(w http.ResponseWriter, id string) {
	if f.forbid["op"] {
		writeARMError(w, http.StatusForbidden, "AuthorizationFailed", "no operation read")
		return
	}
	op, ok := f.ops[id]
	if !ok {
		writeARMError(w, http.StatusNotFound, "NotFound", "no operation "+id)
		return
	}
	op.polls++
	done := !op.never && op.polls >= f.doneAfter
	if done {
		f.finish(op)
	}
	if f.locationOnly {
		if !done {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if op.errMsg != "" {
			writeARMError(w, http.StatusBadRequest, "OperationFailed", op.errMsg)
			return
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	out := map[string]any{"name": op.id, "status": "InProgress"}
	switch {
	case done && op.errMsg != "":
		out["status"] = "Failed"
		out["error"] = map[string]any{"code": "InternalServerError", "message": op.errMsg}
	case done:
		out["status"] = "Succeeded"
	}
	writeSQLJSON(w, out)
}

func (f *azureFake) finish(op *fakeAZOp) {
	s := f.servers[op.server]
	if s == nil || s.State != "Provisioning" {
		return
	}
	switch {
	case op.errMsg != "":
		s.State = "Disabled"
	case f.finalState != "":
		s.State = f.finalState
	default:
		s.State = "Ready"
	}
}

func (f *azureFake) delete(w http.ResponseWriter, name string) {
	f.calls = append(f.calls, "delete "+name)
	switch {
	case f.forbid["delete"]:
		writeARMError(w, http.StatusForbidden, "AuthorizationFailed", "no delete")
		return
	case f.deleteStatus != 0:
		writeARMError(w, f.deleteStatus, "InternalServerError", "delete failed")
		return
	}
	if _, ok := f.servers[name]; !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	delete(f.servers, name)
	w.WriteHeader(http.StatusOK)
}

func (f *azureFake) exists(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.servers[name]
	return ok
}

func (f *azureFake) server(name string) fakeAZServer {
	f.mu.Lock()
	defer f.mu.Unlock()
	return *f.servers[name]
}

func (f *azureFake) put1(s fakeAZServer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.servers[s.Name] = &s
}

func (f *azureFake) edit(name string, edit func(*fakeAZServer)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	edit(f.servers[name])
}

// mutating is every PUT and DELETE, in order.
func (f *azureFake) mutating() []string {
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

func (f *azureFake) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *azureFake) body(key string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bodies[key]
}

func (f *azureFake) header(key string) http.Header {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.headers[key]
}

func (f *azureFake) foreignPolls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.asyncHits
}

func (f *azureFake) pollAuth() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.opPollAuth...)
}

func azConfig(base string) clone.AzureRestoreConfig {
	return clone.AzureRestoreConfig{SubscriptionID: azSub, ResourceGroup: azGroup,
		Server: azServer, Deadline: 2 * time.Hour, TTL: 3*time.Hour + 15*time.Minute,
		BaseURL: base, PollEvery: time.Millisecond, Now: func() time.Time { return azNow },
		Token: func(context.Context) (string, error) { return azToken, nil },
		Production: func(context.Context) (clone.AzureProduction, error) {
			return clone.AzureProduction{DSN: azProdDSN}, nil
		}}
}

func newAzureAPI(t *testing.T, base string,
	edit func(*clone.AzureRestoreConfig)) *clone.AzureRestoreAPI {
	t.Helper()
	cfg := azConfig(base)
	if edit != nil {
		edit(&cfg)
	}
	api, err := clone.NewAzureRestoreAPI(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return api
}

func newAzureProvider(t *testing.T, base string,
	edit func(*clone.AzureRestoreConfig)) *clone.SnapshotProvider {
	t.Helper()
	p, err := clone.NewSnapshotProvider(newAzureAPI(t, base, edit),
		func() time.Time { return azNow })
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func createAZ(t *testing.T, base string, edit func(*clone.AzureRestoreConfig),
) (clone.Clone, error) {
	t.Helper()
	return newAzureProvider(t, base, edit).Create(context.Background(),
		clone.CloneSpec{ResourceName: azName})
}
