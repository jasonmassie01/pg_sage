package clone_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/clone"
)

// lab-05g: how azure_restore fails. A PUT ARM refused made nothing; a
// server that exists and then fails (its restore, its state, its DSN) is
// deleted; one not ready at the restore deadline is left to the reaper
// (create_uncertain); one pg_sage cannot prove is its own is never deleted.

// Error propagation: flexible server names are global DNS names, so a
// name another subscription holds is a 409 that names the conflict; it
// made nothing, and nothing is deleted.
func TestAzureRestore_NameInUseIsAConflictNamingTheName(t *testing.T) {
	f, base := newAzureFake(t)
	f.takenGlobally[azName] = true
	_, err := createAZ(t, base, nil)
	var status *clone.ProviderStatusError
	if !errors.Is(err, clone.ErrAzureAlreadyExists) || !errors.As(err, &status) ||
		status.StatusCode != http.StatusConflict || !strings.Contains(err.Error(), azName) ||
		!strings.Contains(err.Error(), "ServerNameAlreadyExists") ||
		errors.Is(err, clone.ErrCloneLeftBehind) {
		t.Fatalf("err = %v; want a 409 naming %s", err, azName)
	}
	if got := strings.Join(f.mutating(), "|"); got != "put "+azName {
		t.Fatalf("mutating %q, want the one refused PUT", got)
	}
}

// A server of the name that appears between the free-name check and the
// PUT meets If-None-Match: * as a 412: the same conflict, nothing deleted.
func TestAzureRestore_PreconditionFailedPutIsANameConflict(t *testing.T) {
	f, base := newAzureFake(t)
	f.putStatus = http.StatusPreconditionFailed
	_, err := createAZ(t, base, nil)
	var status *clone.ProviderStatusError
	if !errors.Is(err, clone.ErrAzureAlreadyExists) || !errors.As(err, &status) ||
		status.StatusCode != http.StatusPreconditionFailed ||
		!strings.Contains(err.Error(), azName) || errors.Is(err, clone.ErrCloneLeftBehind) {
		t.Fatalf("err = %v; want a 412 name conflict naming %s", err, azName)
	}
	if got := strings.Join(f.mutating(), "|"); got != "put "+azName {
		t.Fatalf("mutating %q, want the one refused PUT and no delete", got)
	}
}

// ARM's PUT is create-or-update: a server of the clone's name in the
// resource group is never PUT to, even one pg_sage tagged.
func TestAzureRestore_ExistingServerOfTheNameIsNeverPut(t *testing.T) {
	f, base := newAzureFake(t)
	f.put1(fakeAZServer{Name: azName, State: "Ready", Role: "Primary",
		FQDN: azName + azSuffix, Version: "16",
		Tags: map[string]string{clone.AzureReceiptTag: azName}})
	_, err := createAZ(t, base, nil)
	if !errors.Is(err, clone.ErrAzureAlreadyExists) || len(f.mutating()) != 0 ||
		!f.exists(azName) || f.server(azName).Body != nil {
		t.Fatalf("err = %v, mutating %q; want refused before any PUT", err, f.mutating())
	}
}

func TestAzureRestore_RefusedPutMadeNothing(t *testing.T) {
	for _, code := range []int{400, 404, 422} {
		f, base := newAzureFake(t)
		f.putStatus = code
		_, err := createAZ(t, base, nil)
		var status *clone.ProviderStatusError
		if !errors.As(err, &status) || status.StatusCode != code ||
			errors.Is(err, clone.ErrCloneLeftBehind) {
			t.Errorf("PUT %d: err = %v", code, err)
		}
		if got := strings.Join(f.mutating(), "|"); got != "put "+azName {
			t.Errorf("PUT %d: mutating %q, want no delete", code, got)
		}
	}
}

// A 5xx may follow a PUT ARM accepted: the server it made is deleted.
func TestAzureRestore_ServerErrorAfterThePutDeletesTheServer(t *testing.T) {
	f, base := newAzureFake(t)
	f.putThenCode = http.StatusInternalServerError
	_, err := createAZ(t, base, nil)
	var status *clone.ProviderStatusError
	if !errors.As(err, &status) || status.StatusCode != 500 || f.exists(azName) ||
		errors.Is(err, clone.ErrCloneLeftBehind) {
		t.Fatalf("err = %v, server exists %v; want it deleted", err, f.exists(azName))
	}
	if got := strings.Join(f.mutating(), "|"); got != "put "+azName+"|delete "+azName {
		t.Fatalf("mutating %q", got)
	}
}

// A 5xx with no server described yet may still have been accepted: the
// create stays uncertain (left behind), so the reaper looks again.
func TestAzureRestore_LostPutWithNoServerYetStaysUncertain(t *testing.T) {
	f, base := newAzureFake(t)
	f.putStatus = http.StatusServiceUnavailable
	_, err := createAZ(t, base, nil)
	if !errors.Is(err, clone.ErrCloneLeftBehind) || !strings.Contains(err.Error(),
		"may have been accepted") || len(f.mutating()) != 1 {
		t.Fatalf("err = %v, mutating %q; want left behind, no delete", err, f.mutating())
	}
}

func TestAzureRestore_FailedOperationDeletesTheServer(t *testing.T) {
	f, base := newAzureFake(t)
	f.putOpError = "restore failed: the point in time is not available"
	_, err := createAZ(t, base, nil)
	if err == nil || !strings.Contains(err.Error(), "point in time is not available") ||
		f.exists(azName) || errors.Is(err, clone.ErrCloneLeftBehind) {
		t.Fatalf("err = %v, exists %v", err, f.exists(azName))
	}
	f, base = newAzureFake(t)
	f.putOpError, f.locationOnly = "out of capacity", true
	if _, err := createAZ(t, base, nil); err == nil || f.exists(azName) {
		t.Fatalf("Location-polled failure: err = %v, exists %v", err, f.exists(azName))
	}
}

// Boundary: a restore still running at the restore deadline is left to
// the reaper, never deleted mid-flight.
func TestAzureRestore_OperationPastTheDeadlineIsLeftForTheReaper(t *testing.T) {
	f, base := newAzureFake(t)
	f.putNever = true
	api := newAzureAPI(t, base, nil)
	clone.SetAzureRestoreDeadlineForTest(api, 50*time.Millisecond)
	p, err := clone.NewSnapshotProvider(api, func() time.Time { return azNow })
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Create(context.Background(), clone.CloneSpec{ResourceName: azName})
	if err == nil || !strings.Contains(err.Error(), "reaper") || !f.exists(azName) ||
		!errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, exists %v; want left to the reaper", err, f.exists(azName))
	}
	if got := strings.Join(f.mutating(), "|"); got != "put "+azName {
		t.Fatalf("mutating %q, want no delete", got)
	}
}

// A reply naming an operation URL off the ARM endpoint, or not an
// operation of this subscription, is never polled: the bearer token would
// go with it. The server the PUT made is deleted.
func TestAzureRestore_ForeignOperationURLIsNeverPolled(t *testing.T) {
	var mu sync.Mutex
	var stolen []string
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		stolen = append(stolen, r.Header.Get("Authorization"))
		mu.Unlock()
		writeSQLJSON(w, map[string]any{"status": "Succeeded"})
	}))
	t.Cleanup(evil.Close)
	other := "/subscriptions/99999999-9999-4999-8999-999999999999/providers/" +
		"Microsoft.DBforPostgreSQL/locations/eastus/azureAsyncOperation/op-1"
	for name, opURL := range map[string]func(base string) string{
		"another host":         func(string) string { return evil.URL + azOpPrefix + "op-1" },
		"another subscription": func(base string) string { return base + other },
		"a path out":           func(base string) string { return base + azOpPrefix + "../../x" },
		"a dot-dot id":         func(base string) string { return base + azOpPrefix + ".." },
		"userinfo": func(base string) string {
			return strings.Replace(base, "://", "://u:p@", 1) + azOpPrefix + "op-1"
		},
		"not an operation": func(base string) string {
			return base + azServersID + "other-server"
		},
		"no scheme": func(string) string {
			return "//" + strings.TrimPrefix(evil.URL, "http://") + azOpPrefix + "op-1"
		},
	} {
		f, base := newAzureFake(t)
		f.asyncURL = opURL(base)
		_, err := createAZ(t, base, nil)
		if err == nil || f.exists(azName) || len(f.pollAuth()) != 0 || f.foreignPolls() != 0 {
			t.Errorf("%s: err = %v, exists %v, polls %d/%d; want refused and deleted", name,
				err, f.exists(azName), len(f.pollAuth()), f.foreignPolls())
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(stolen) != 0 {
		t.Fatalf("a foreign operation URL got %d requests", len(stolen))
	}
}

// Error propagation: a 401 or 403 is ErrAzurePermission naming the action
// the call needs and the resource group.
func TestAzureRestore_PermissionDeniedNamesTheAction(t *testing.T) {
	for knob, action := range map[string]string{
		"read":  "Microsoft.DBforPostgreSQL/flexibleServers/read",
		"write": "Microsoft.DBforPostgreSQL/flexibleServers/write",
		"op":    "Microsoft.DBforPostgreSQL/locations/azureAsyncOperation/read",
	} {
		f, base := newAzureFake(t)
		f.forbid[knob] = true
		_, err := createAZ(t, base, nil)
		if !errors.Is(err, clone.ErrAzurePermission) || !strings.Contains(err.Error(), action) ||
			!strings.Contains(err.Error(), azGroup) {
			t.Errorf("%s denied: err = %v; want it to name %s on %s", knob, err, action, azGroup)
		}
	}
	f, base := newAzureFake(t)
	c, err := createAZ(t, base, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.forbid["delete"] = true
	err = newAzureProvider(t, base, nil).Destroy(context.Background(), c)
	if !errors.Is(err, clone.ErrAzurePermission) || !f.exists(azName) ||
		!strings.Contains(err.Error(), "Microsoft.DBforPostgreSQL/flexibleServers/delete") {
		t.Fatalf("delete denied: err = %v", err)
	}
}

func TestAzureRestore_NotReadyAfterTheRestoreDeletesIt(t *testing.T) {
	f, base := newAzureFake(t)
	f.finalState = "Disabled"
	_, err := createAZ(t, base, nil)
	if err == nil || !strings.Contains(err.Error(), "Disabled") || f.exists(azName) {
		t.Fatalf("err = %v, exists %v; want deleted", err, f.exists(azName))
	}
}

// A restored server without its receipt tag is never deleted: pg_sage
// cannot prove it is its own, and says how to delete it by hand.
func TestAzureRestore_UntaggedRestoreIsLeftBehindNotDeleted(t *testing.T) {
	f, base := newAzureFake(t)
	f.dropTags = true
	_, err := createAZ(t, base, nil)
	if !errors.Is(err, clone.ErrCloneLeftBehind) || !f.exists(azName) ||
		!strings.Contains(err.Error(), "az postgres flexible-server delete") {
		t.Fatalf("err = %v, exists %v; want left behind, not deleted", err, f.exists(azName))
	}
	if got := strings.Join(f.mutating(), "|"); got != "put "+azName {
		t.Fatalf("mutating %q, want no delete", got)
	}
}

func TestAzureRestore_CloneOnProductionsFQDNIsDeleted(t *testing.T) {
	f, base := newAzureFake(t)
	f.cloneFQDN = strings.ToUpper(azProdFQDN)
	_, err := createAZ(t, base, nil)
	if !errors.Is(err, clone.ErrCloneIsProduction) || f.exists(azName) || !f.exists(azServer) {
		t.Fatalf("err = %v, clone exists %v", err, f.exists(azName))
	}
}

// Destroy deletes only a sage-* server tagged with its own name; one gone
// or already dropping counts as deleted.
func TestAzureRestore_DestroyDeletesOnlyPgSageServers(t *testing.T) {
	f, base := newAzureFake(t)
	p := newAzureProvider(t, base, nil)
	f.put1(fakeAZServer{Name: "sage-rehearsal-bbbbbbbb", State: "Ready", Tags: nil})
	f.put1(fakeAZServer{Name: "sage-rehearsal-cccccccc", State: "Ready",
		Tags: map[string]string{clone.AzureReceiptTag: "sage-rehearsal-dddddddd"}})
	f.put1(fakeAZServer{Name: "sage-rehearsal-eeeeeeee", State: "Dropping",
		Tags: map[string]string{clone.AzureReceiptTag: "sage-rehearsal-eeeeeeee"}})
	for _, id := range []string{azServer, "sage-rehearsal-bbbbbbbb",
		"sage-rehearsal-cccccccc"} {
		if err := p.Destroy(context.Background(), clone.Clone{ID: id}); !errors.Is(err,
			clone.ErrInvalidAzureRestoreConfig) || !f.exists(id) {
			t.Errorf("destroy %s: err = %v, exists %v; want refused", id, err, f.exists(id))
		}
	}
	for _, id := range []string{"sage-rehearsal-eeeeeeee", "sage-rehearsal-ffffffff"} {
		if err := p.Destroy(context.Background(), clone.Clone{ID: id}); err != nil {
			t.Errorf("destroy %s: %v; want deleted", id, err)
		}
	}
	if len(f.mutating()) != 0 {
		t.Fatalf("mutating %q, want no delete call", f.mutating())
	}
}

func TestAzureRestore_DeleteFailureIsAnError(t *testing.T) {
	f, base := newAzureFake(t)
	c, err := createAZ(t, base, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.deleteStatus = http.StatusInternalServerError
	err = newAzureProvider(t, base, nil).Destroy(context.Background(), c)
	var status *clone.ProviderStatusError
	if !errors.As(err, &status) || status.StatusCode != 500 || !f.exists(azName) {
		t.Fatalf("err = %v, exists %v", err, f.exists(azName))
	}
	f, base = newAzureFake(t)
	f.putOpError, f.deleteStatus = "failed", http.StatusConflict
	_, err = createAZ(t, base, nil)
	if !errors.Is(err, clone.ErrCloneLeftBehind) || !f.exists(azName) ||
		errors.Is(err, clone.ErrAzureAlreadyExists) {
		t.Fatalf("cleanup delete 409: err = %v; want left behind, not already-exists", err)
	}
}

// FindByName: a tagged server is found; one gone is not found; one of the
// name not tagged for this receipt is an error, never "not found", so the
// reaper keeps its receipt open.
func TestAzureRestore_FindByName(t *testing.T) {
	f, base := newAzureFake(t)
	api := newAzureAPI(t, base, nil)
	f.put1(fakeAZServer{Name: "sage-rehearsal-aaaaaaaa", State: "Ready",
		Tags: map[string]string{clone.AzureReceiptTag: "sage-rehearsal-aaaaaaaa"}})
	f.put1(fakeAZServer{Name: "sage-rehearsal-bbbbbbbb", State: "Ready"})
	ctx := context.Background()
	if ref, found, err := api.FindByName(ctx, "sage-rehearsal-aaaaaaaa"); err != nil ||
		!found || ref != "sage-rehearsal-aaaaaaaa" {
		t.Fatalf("tagged: %q, %v, %v", ref, found, err)
	}
	if ref, found, err := api.FindByName(ctx, "sage-rehearsal-99999999"); err != nil ||
		found || ref != "" {
		t.Fatalf("gone: %q, %v, %v", ref, found, err)
	}
	ref, found, err := api.FindByName(ctx, "sage-rehearsal-bbbbbbbb")
	if !errors.Is(err, clone.ErrCloneLeftBehind) || found || ref != "" {
		t.Fatalf("untagged: %q, %v, %v; want left behind", ref, found, err)
	}
	if _, _, err := api.FindByName(ctx, "shop"); !errors.Is(err,
		clone.ErrInvalidAzureRestoreConfig) {
		t.Fatalf("non-clone name: %v", err)
	}
	if len(f.mutating()) != 0 {
		t.Fatalf("mutating %q", f.mutating())
	}
}

// No error quotes the bearer token or the production password.
func TestAzureRestore_ErrorsNeverQuoteSecrets(t *testing.T) {
	for _, edit := range []func(*azureFake){
		func(f *azureFake) { f.forbid["write"] = true },
		func(f *azureFake) { f.putOpError = "failed" },
		func(f *azureFake) { f.cloneFQDN = azProdFQDN },
		func(f *azureFake) { f.getStatus = 500 },
	} {
		f, base := newAzureFake(t)
		edit(f)
		_, err := createAZ(t, base, nil)
		if err == nil || strings.Contains(err.Error(), azToken) ||
			strings.Contains(err.Error(), azPassword) {
			t.Errorf("err = %v; want an error without the token or password", err)
		}
	}
}

// Boundary: a caller that ends before the restore deadline (a bounded MCP
// call) would cut the wait short and leave a billable copy to the reaper
// every time, so nothing is made; a snapshot of another server or of the
// future is refused before any PUT.
func TestAzureRestore_RestoreRefusesShortCallersAndForeignSnapshots(t *testing.T) {
	f, base := newAzureFake(t)
	api := newAzureAPI(t, base, nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	snap := clone.Snapshot{ID: azSourceID, CreatedAt: azNow.Add(-clone.AzureRestoreLag)}
	if _, err := api.Restore(ctx, snap, clone.CloneSpec{ResourceName: azName}); !errors.Is(
		err, clone.ErrCloneUnavailable) || !strings.Contains(err.Error(), "restore deadline") ||
		f.callCount() != 0 {
		t.Fatalf("1h caller, 2h deadline: err = %v, calls %d", err, f.callCount())
	}
	for name, s := range map[string]clone.Snapshot{
		"another server": {ID: azServersID + "other", CreatedAt: snap.CreatedAt},
		"the future":     {ID: azSourceID, CreatedAt: azNow.Add(time.Minute)},
	} {
		_, err := api.Restore(context.Background(), s, clone.CloneSpec{ResourceName: azName})
		if !errors.Is(err, clone.ErrInvalidAzureRestoreConfig) || len(f.mutating()) != 0 {
			t.Errorf("%s: err = %v, mutating %q; want refused", name, err, f.mutating())
		}
	}
	upper := clone.Snapshot{ID: strings.ToUpper(azSourceID), CreatedAt: snap.CreatedAt}
	if c, err := api.Restore(context.Background(), upper,
		clone.CloneSpec{ResourceName: azName}); err != nil || c.ID != azName {
		t.Fatalf("resource ID in another case: %+v, %v", c, err)
	}
}
