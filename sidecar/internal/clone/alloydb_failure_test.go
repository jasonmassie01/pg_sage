package clone_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/clone"
)

// lab-05f: how alloydb_restore fails. A restore AlloyDB refused made
// nothing; a cluster that exists and then fails (its restore, its primary
// instance, its DSN) is deleted with its instances; one not ready at the
// restore deadline is left to the reaper (create_uncertain).

// Error propagation: the primary instance failing after the cluster
// exists deletes the cluster (force: with any instance it has).
func TestAlloyDB_InstanceCreateFailingDeletesTheCluster(t *testing.T) {
	for _, code := range []int{400, 409, 500, 503} {
		f, base := newAlloyDBFake(t)
		f.instanceStatus = code
		_, err := createADB(t, base, nil)
		var status *clone.ProviderStatusError
		if !errors.As(err, &status) || status.StatusCode != code || f.exists(adbName) ||
			errors.Is(err, clone.ErrCloneLeftBehind) {
			t.Errorf("instance %d: err = %v, cluster exists %v", code, err, f.exists(adbName))
		}
		want := "restore " + adbName + "|create instance " + adbName + "/" + adbName +
			"|delete " + adbName + " force=true"
		if got := strings.Join(f.mutating(), "|"); got != want {
			t.Errorf("instance %d: mutating %q, want %q", code, got, want)
		}
	}
}

func TestAlloyDB_FailedInstanceOperationDeletesTheCluster(t *testing.T) {
	f, base := newAlloyDBFake(t)
	f.instanceOpError = "instance failed: out of capacity"
	_, err := createADB(t, base, nil)
	if err == nil || !strings.Contains(err.Error(), "out of capacity") || f.exists(adbName) {
		t.Fatalf("err = %v, exists %v", err, f.exists(adbName))
	}
	if _, ok := f.instance(adbName, adbName); ok {
		t.Fatal("the failed instance outlived its cluster")
	}
}

func TestAlloyDB_FailedRestoreOperationDeletesTheCluster(t *testing.T) {
	f, base := newAlloyDBFake(t)
	f.restoreOpError = "restore failed: point in time unavailable"
	_, err := createADB(t, base, nil)
	if err == nil || !strings.Contains(err.Error(), "point in time unavailable") ||
		f.exists(adbName) {
		t.Fatalf("err = %v, exists %v", err, f.exists(adbName))
	}
	if strings.Contains(strings.Join(f.mutating(), "|"), "create instance") {
		t.Fatalf("an instance was asked for on a failed restore: %q", f.mutating())
	}
}

// The restore ignoring the receipt labels leaves a cluster pg_sage cannot
// prove is its own: it is never deleted, and the error says it is left.
func TestAlloyDB_UnlabelledRestoredClusterIsLeftBehindNotDeleted(t *testing.T) {
	f, base := newAlloyDBFake(t)
	f.dropLabels = true
	_, err := createADB(t, base, nil)
	if !errors.Is(err, clone.ErrCloneLeftBehind) || !f.exists(adbName) ||
		!strings.Contains(err.Error(), clone.AlloyDBReceiptLabel) {
		t.Fatalf("err = %v, exists %v", err, f.exists(adbName))
	}
	if got := strings.Join(f.mutating(), "|"); got != "restore "+adbName {
		t.Fatalf("mutating %q, want only the restore", got)
	}
}

// Boundary: a restore or instance operation still running at the restore
// deadline is an uncertain create, and nothing is deleted.
func TestAlloyDB_OperationPastTheDeadlineIsLeftForTheReaper(t *testing.T) {
	for _, which := range []string{"restore", "instance"} {
		f, base := newAlloyDBFake(t)
		f.restoreNever = which == "restore"
		f.instanceNever = which == "instance"
		api := newAlloyDBAPI(t, base, nil)
		clone.SetAlloyDBDeadlineForTest(api, 50*time.Millisecond)
		p, err := clone.NewSnapshotProvider(api, func() time.Time { return adbNow })
		if err != nil {
			t.Fatal(err)
		}
		_, err = p.Create(context.Background(), clone.CloneSpec{ResourceName: adbName})
		if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "reaper") {
			t.Errorf("%s: err = %v, want a deadline left to the reaper", which, err)
		}
		if !f.exists(adbName) || strings.Contains(strings.Join(f.mutating(), "|"), "delete") {
			t.Errorf("%s: mutating %q, exists %v: want the clone left alone", which,
				f.mutating(), f.exists(adbName))
		}
	}
}

// A caller that ends before the restore deadline would always abandon the
// full copy: refused before the restore is asked for.
func TestAlloyDB_CallerEndingBeforeTheDeadlineIsRefused(t *testing.T) {
	f, base := newAlloyDBFake(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	_, err := newAlloyDBProvider(t, base, nil).Create(ctx,
		clone.CloneSpec{ResourceName: adbName})
	if !errors.Is(err, clone.ErrCloneUnavailable) ||
		!strings.Contains(err.Error(), "clone.restore_deadline_minutes") ||
		len(f.mutating()) != 0 {
		t.Fatalf("err = %v, mutating %q; want unavailable, nothing asked", err, f.mutating())
	}
}

// Error propagation: a 403 names the IAM permission that is missing.
func TestAlloyDB_ForbiddenNamesTheMissingPermission(t *testing.T) {
	for op, perm := range map[string]string{"restore": "alloydb.clusters.restore",
		"get": "alloydb.clusters.get", "list": "alloydb.instances.list",
		"instance": "alloydb.instances.create", "op": "alloydb.operations.get"} {
		f, base := newAlloyDBFake(t)
		f.forbid[op] = true
		_, err := createADB(t, base, nil)
		if !errors.Is(err, clone.ErrAlloyDBPermission) || !strings.Contains(err.Error(), perm) ||
			!strings.Contains(err.Error(), adbProject) {
			t.Errorf("forbidden %s: err = %v, want %s named", op, err, perm)
		}
		if op == "instance" && f.exists(adbName) {
			t.Errorf("forbidden instance create left the cluster")
		}
	}
	f, base := newAlloyDBFake(t)
	f.forbid["delete"] = true
	f.put(fakeADBCluster{Name: adbName, State: "READY",
		Labels: map[string]string{clone.AlloyDBReceiptLabel: adbName}})
	err := newAlloyDBProvider(t, base, nil).Destroy(context.Background(),
		clone.Clone{ID: adbName})
	if !errors.Is(err, clone.ErrAlloyDBPermission) ||
		!strings.Contains(err.Error(), "alloydb.clusters.delete") || !f.exists(adbName) {
		t.Fatalf("forbidden delete: err = %v", err)
	}
}

func TestAlloyDB_RefusedRestoreMadeNothing(t *testing.T) {
	f, base := newAlloyDBFake(t)
	f.restoreStatus = 400
	_, err := createADB(t, base, nil)
	var status *clone.ProviderStatusError
	if !errors.As(err, &status) || status.StatusCode != 400 ||
		errors.Is(err, clone.ErrCloneLeftBehind) || f.exists(adbName) ||
		len(f.mutating()) != 1 {
		t.Fatalf("err = %v, mutating %q", err, f.mutating())
	}
}

// A cluster of that name already exists: it is not pg_sage's, so it is
// never deleted.
func TestAlloyDB_ExistingClusterOfTheNameIsNeverDeleted(t *testing.T) {
	f, base := newAlloyDBFake(t)
	f.put(fakeADBCluster{Name: adbName, State: "READY",
		Labels: map[string]string{clone.AlloyDBReceiptLabel: adbName}})
	_, err := createADB(t, base, nil)
	if !errors.Is(err, clone.ErrAlloyDBAlreadyExists) || !f.exists(adbName) ||
		len(f.mutating()) != 1 {
		t.Fatalf("err = %v, mutating %q", err, f.mutating())
	}
}

// A 5xx after AlloyDB accepted the restore: pg_sage looks the cluster up
// by name and deletes it.
func TestAlloyDB_LostRestoreReplyIsFoundAndDeleted(t *testing.T) {
	f, base := newAlloyDBFake(t)
	f.restoreThenCode = 503
	_, err := createADB(t, base, nil)
	if err == nil || f.exists(adbName) || errors.Is(err, clone.ErrCloneLeftBehind) {
		t.Fatalf("err = %v, exists %v, mutating %q", err, f.exists(adbName), f.mutating())
	}
	if !f.exists(adbSource) {
		t.Fatal("cleanup touched the source")
	}
}

// A lost reply whose cluster AlloyDB does not describe yet stays uncertain.
func TestAlloyDB_LostRestoreReplyNotYetDescribedIsLeftBehind(t *testing.T) {
	f, base := newAlloyDBFake(t)
	f.restoreStatus = 502 // refused before anything was made, but pg_sage cannot know
	_, err := createADB(t, base, nil)
	if !errors.Is(err, clone.ErrCloneLeftBehind) || f.exists(adbName) ||
		!strings.Contains(err.Error(), "does not describe") {
		t.Fatalf("err = %v, exists %v", err, f.exists(adbName))
	}
}

// A cluster pg_sage cannot delete is reported left behind, so the receipt
// is create_uncertain and the reaper looks again.
func TestAlloyDB_CleanupFailureIsLeftBehind(t *testing.T) {
	f, base := newAlloyDBFake(t)
	f.instanceStatus = 500
	f.deleteStatus = 500
	_, err := createADB(t, base, nil)
	if !errors.Is(err, clone.ErrCloneLeftBehind) || !f.exists(adbName) ||
		!strings.Contains(err.Error(), "expires at") {
		t.Fatalf("err = %v, exists %v", err, f.exists(adbName))
	}
}

func TestAlloyDB_CloneOnProductionsIPIsDeleted(t *testing.T) {
	f, base := newAlloyDBFake(t)
	f.cloneIP = adbProdIP
	_, err := createADB(t, base, nil)
	if !errors.Is(err, clone.ErrCloneIsProduction) || f.exists(adbName) {
		t.Fatalf("err = %v, exists %v", err, f.exists(adbName))
	}
}

func TestAlloyDB_SourceMustBeTheMonitoredPrimaryCluster(t *testing.T) {
	cases := map[string]func(*alloyDBFake, *clone.AlloyDBRestoreConfig){
		"host not the primary's IP": func(_ *alloyDBFake, c *clone.AlloyDBRestoreConfig) {
			c.Production = func(context.Context) (clone.AlloyDBProduction, error) {
				return clone.AlloyDBProduction{DSN: "postgres://app@127.0.0.1:5432/shop"}, nil
			}
		},
		"host is the read pool": func(_ *alloyDBFake, c *clone.AlloyDBRestoreConfig) {
			c.Production = func(context.Context) (clone.AlloyDBProduction, error) {
				return clone.AlloyDBProduction{DSN: "postgres://app@10.0.0.6:5432/shop"}, nil
			}
		},
		"secondary cluster": func(f *alloyDBFake, _ *clone.AlloyDBRestoreConfig) {
			f.clusters[adbSource].Type = "SECONDARY"
		},
		"not postgres": func(f *alloyDBFake, _ *clone.AlloyDBRestoreConfig) {
			f.clusters[adbSource].Version = ""
		},
		"missing": func(_ *alloyDBFake, c *clone.AlloyDBRestoreConfig) { c.Cluster = "gone" },
	}
	for name, edit := range cases {
		f, base := newAlloyDBFake(t)
		_, err := createADB(t, base, func(c *clone.AlloyDBRestoreConfig) { edit(f, c) })
		if !errors.Is(err, clone.ErrNotAlloyDBCluster) || len(f.mutating()) != 0 {
			t.Errorf("%s: err = %v, mutating %q", name, err, f.mutating())
		}
	}
	f, base := newAlloyDBFake(t)
	f.clusters[adbSource].State = "MAINTENANCE"
	if _, err := createADB(t, base, nil); !errors.Is(err, clone.ErrCloneUnavailable) ||
		len(f.mutating()) != 0 {
		t.Fatalf("source in maintenance: err = %v", err)
	}
}

func TestAlloyDB_ErrorsNeverQuoteTheDSNOrToken(t *testing.T) {
	for _, op := range []string{"restore", "instance"} {
		f, base := newAlloyDBFake(t)
		f.forbid[op] = true
		_, err := createADB(t, base, nil)
		if err == nil || strings.Contains(err.Error(), adbPassword) ||
			strings.Contains(err.Error(), adbToken) {
			t.Fatalf("%s: err = %v", op, err)
		}
	}
}

func TestAlloyDB_DestroyDeletesOnlyPgSageClusters(t *testing.T) {
	f, base := newAlloyDBFake(t)
	p := newAlloyDBProvider(t, base, nil)
	other := "sage-rehearsal-0000cccc"
	f.put(fakeADBCluster{Name: other, State: "READY",
		Labels: map[string]string{clone.AlloyDBReceiptLabel: "sage-rehearsal-0000dddd"}})
	unlabelled := "sage-rehearsal-0000eeee"
	f.put(fakeADBCluster{Name: unlabelled, State: "READY", Labels: map[string]string{}})
	for _, id := range []string{adbSource, other, unlabelled, "", "sage-Rehearsal-0000eeee"} {
		if err := p.Destroy(context.Background(), clone.Clone{ID: id}); err == nil {
			t.Errorf("destroy %q succeeded", id)
		}
	}
	if len(f.mutating()) != 0 || !f.exists(other) || !f.exists(unlabelled) {
		t.Fatalf("mutating calls %q", f.mutating())
	}
	if err := p.Destroy(context.Background(), clone.Clone{ID: adbName}); err != nil {
		t.Fatalf("destroy of a gone clone: %v", err)
	}
	f.put(fakeADBCluster{Name: adbName, State: "DELETING",
		Labels: map[string]string{clone.AlloyDBReceiptLabel: adbName}})
	if err := p.Destroy(context.Background(), clone.Clone{ID: adbName}); err != nil ||
		len(f.mutating()) != 0 {
		t.Fatalf("destroy while deleting: %v, mutating %q", err, f.mutating())
	}
}

// State transitions of a lookup: labelled, unlabelled (not provably
// pg_sage's), labelled for another receipt, and gone.
func TestAlloyDB_FindByName(t *testing.T) {
	f, base := newAlloyDBFake(t)
	api := newAlloyDBAPI(t, base, nil)
	f.put(fakeADBCluster{Name: "sage-rehearsal-0000aaaa", State: "CREATING",
		Labels: map[string]string{clone.AlloyDBReceiptLabel: "sage-rehearsal-0000aaaa"}})
	f.put(fakeADBCluster{Name: "sage-rehearsal-0000bbbb", State: "READY",
		Labels: map[string]string{"team": "shop"}})
	f.put(fakeADBCluster{Name: "sage-rehearsal-0000cccc", State: "READY",
		Labels: map[string]string{clone.AlloyDBReceiptLabel: "sage-rehearsal-0000dddd"}})
	want := map[string]bool{"sage-rehearsal-0000aaaa": true, "sage-rehearsal-0000bbbb": false,
		"sage-rehearsal-0000cccc": false, "sage-rehearsal-0000ffff": false}
	for name, found := range want {
		ref, ok, err := api.FindByName(context.Background(), name)
		if err != nil || ok != found || (found && ref != name) || (!found && ref != "") {
			t.Errorf("find %s = %q, %v, %v; want found %v", name, ref, ok, err, found)
		}
	}
	if _, _, err := api.FindByName(context.Background(), "shop"); !errors.Is(err,
		clone.ErrInvalidAlloyDBConfig) {
		t.Fatalf("find shop: %v", err)
	}
	f.getStatus = 500
	if _, _, err := api.FindByName(context.Background(), adbName); err == nil {
		t.Fatal("a failed lookup reported not found")
	}
}

// A 409 on delete is another operation in progress on the cluster, never
// "already exists": the error keeps the status and nothing is deleted.
func TestAlloyDB_ConflictOnDeleteIsAnOperationInProgress(t *testing.T) {
	f, base := newAlloyDBFake(t)
	f.put(fakeADBCluster{Name: adbName, State: "READY",
		Labels: map[string]string{clone.AlloyDBReceiptLabel: adbName}})
	f.deleteStatus = 409
	err := newAlloyDBAPI(t, base, nil).Destroy(context.Background(), adbName)
	var status *clone.ProviderStatusError
	if !errors.As(err, &status) || status.StatusCode != 409 ||
		errors.Is(err, clone.ErrAlloyDBAlreadyExists) ||
		!strings.Contains(err.Error(), "another operation") || !f.exists(adbName) {
		t.Fatalf("err = %v, want a 409 operation-in-progress status error", err)
	}
}
