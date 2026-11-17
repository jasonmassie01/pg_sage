package clone_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/clone"
)

// lab-05e: how cloudsql_clone fails. A clone Cloud SQL refused made
// nothing; one it accepted and that then failed is deleted; one still not
// done at the restore deadline is left to the reaper (create_uncertain).

func createSQL(t *testing.T, base string, edit func(*clone.CloudSQLCloneConfig),
) (clone.Clone, error) {
	t.Helper()
	return newCloudSQLProvider(t, base, edit).Create(context.Background(),
		clone.CloneSpec{ResourceName: sqlName})
}

// Boundary: an operation still running at the restore deadline is an
// uncertain create (a context deadline), and the clone is not deleted.
func TestCloudSQL_PollPastTheDeadlineIsLeftForTheReaper(t *testing.T) {
	f, base := newCloudSQLFake(t)
	f.neverDone = true
	api := newCloudSQLAPI(t, base, nil)
	clone.SetCloudSQLDeadlineForTest(api, 50*time.Millisecond)
	p, err := clone.NewSnapshotProvider(api, func() time.Time { return sqlNow })
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Create(context.Background(), clone.CloneSpec{ResourceName: sqlName})
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "reaper") {
		t.Fatalf("err = %v, want a deadline left to the reaper", err)
	}
	if !f.exists(sqlName) || strings.Join(f.mutating(), "|") != "clone shop to "+sqlName {
		t.Fatalf("mutating calls %q, exists %v: want the clone left alone", f.mutating(),
			f.exists(sqlName))
	}
}

// A caller that ends before the restore deadline (a bounded MCP call)
// would always abandon the full copy: refused before the clone is asked
// for (only the read-only source lookup of LatestSnapshot runs).
func TestCloudSQL_CallerEndingBeforeTheDeadlineIsRefused(t *testing.T) {
	f, base := newCloudSQLFake(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	_, err := newCloudSQLProvider(t, base, nil).Create(ctx,
		clone.CloneSpec{ResourceName: sqlName})
	if !errors.Is(err, clone.ErrCloneUnavailable) ||
		!strings.Contains(err.Error(), "clone.restore_deadline_minutes") ||
		len(f.mutating()) != 0 || f.callCount() != 1 {
		t.Fatalf("err = %v, calls %d (mutating %q); want unavailable after one read", err,
			f.callCount(), f.mutating())
	}
}

// Error propagation: a 403 names the IAM permission that is missing.
func TestCloudSQL_ForbiddenNamesTheMissingPermission(t *testing.T) {
	for op, perm := range map[string]string{"clone": "cloudsql.instances.clone",
		"get": "cloudsql.instances.get", "patch": "cloudsql.instances.update"} {
		f, base := newCloudSQLFake(t)
		f.instances[sqlSource].Protected = false
		f.forbid[op] = true
		_, err := createSQL(t, base, nil)
		if !errors.Is(err, clone.ErrCloudSQLPermission) || !strings.Contains(err.Error(), perm) ||
			!strings.Contains(err.Error(), sqlProject) {
			t.Errorf("forbidden %s: err = %v, want %s named", op, err, perm)
		}
		if op == "patch" && f.exists(sqlName) {
			t.Errorf("forbidden patch left the clone")
		}
	}
	f, base := newCloudSQLFake(t)
	f.forbid["delete"] = true
	f.put(fakeSQLInstance{Name: sqlName, State: "RUNNABLE",
		Labels: map[string]string{clone.CloudSQLReceiptLabel: sqlName}})
	err := newCloudSQLProvider(t, base, nil).Destroy(context.Background(),
		clone.Clone{ID: sqlName})
	if !errors.Is(err, clone.ErrCloudSQLPermission) ||
		!strings.Contains(err.Error(), "cloudsql.instances.delete") || !f.exists(sqlName) {
		t.Fatalf("forbidden delete: err = %v", err)
	}
}

func TestCloudSQL_RefusedCloneMadeNothing(t *testing.T) {
	f, base := newCloudSQLFake(t)
	f.cloneStatus = 400
	_, err := createSQL(t, base, nil)
	var status *clone.ProviderStatusError
	if !errors.As(err, &status) || status.StatusCode != 400 ||
		errors.Is(err, clone.ErrCloneLeftBehind) || f.exists(sqlName) ||
		len(f.mutating()) != 1 {
		t.Fatalf("err = %v, mutating %q", err, f.mutating())
	}
}

// An instance of that name already exists: it is not pg_sage's (no retry
// sent the clone twice), so it is never deleted.
func TestCloudSQL_ExistingInstanceOfTheNameIsNeverDeleted(t *testing.T) {
	f, base := newCloudSQLFake(t)
	f.put(fakeSQLInstance{Name: sqlName, State: "RUNNABLE", Labels: map[string]string{}})
	_, err := createSQL(t, base, nil)
	if !errors.Is(err, clone.ErrCloudSQLAlreadyExists) || !f.exists(sqlName) ||
		len(f.mutating()) != 1 {
		t.Fatalf("err = %v, mutating %q", err, f.mutating())
	}
}

// A 5xx after Cloud SQL accepted the clone: pg_sage looks the instance up
// by name and deletes it (unprotecting the clone it made first).
func TestCloudSQL_LostCloneReplyIsFoundAndDeleted(t *testing.T) {
	f, base := newCloudSQLFake(t)
	f.cloneThenCode = 503
	_, err := createSQL(t, base, nil)
	if err == nil || f.exists(sqlName) || errors.Is(err, clone.ErrCloneLeftBehind) {
		t.Fatalf("err = %v, exists %v, mutating %q", err, f.exists(sqlName), f.mutating())
	}
	if !f.exists(sqlSource) || !f.instance(sqlSource).Protected {
		t.Fatal("cleanup touched the source")
	}
}

func TestCloudSQL_FailedOperationDeletesTheClone(t *testing.T) {
	f, base := newCloudSQLFake(t)
	f.opError = "clone failed: out of capacity"
	_, err := createSQL(t, base, nil)
	if err == nil || !strings.Contains(err.Error(), "out of capacity") || f.exists(sqlName) {
		t.Fatalf("err = %v, exists %v", err, f.exists(sqlName))
	}
}

func TestCloudSQL_PatchFailureDeletesTheClone(t *testing.T) {
	f, base := newCloudSQLFake(t)
	f.instances[sqlSource].Protected = false
	f.patchStatus = 500
	_, err := createSQL(t, base, nil)
	if err == nil || f.exists(sqlName) || errors.Is(err, clone.ErrCloneLeftBehind) {
		t.Fatalf("err = %v, exists %v", err, f.exists(sqlName))
	}
}

// A clone pg_sage cannot unprotect or delete is reported left behind, so
// the receipt is create_uncertain and the reaper looks again.
func TestCloudSQL_CleanupFailureIsLeftBehind(t *testing.T) {
	f, base := newCloudSQLFake(t)
	f.patchStatus = 500 // the clone keeps the copied deletion protection
	_, err := createSQL(t, base, nil)
	if !errors.Is(err, clone.ErrCloneLeftBehind) || !f.exists(sqlName) {
		t.Fatalf("err = %v, exists %v", err, f.exists(sqlName))
	}
}

func TestCloudSQL_CloneOnProductionsIPIsDeleted(t *testing.T) {
	f, base := newCloudSQLFake(t)
	f.instances[sqlSource].Protected = false
	f.cloneIP = sqlProdIP
	_, err := createSQL(t, base, nil)
	if !errors.Is(err, clone.ErrCloneIsProduction) || f.exists(sqlName) {
		t.Fatalf("err = %v, exists %v", err, f.exists(sqlName))
	}
}

func TestCloudSQL_SourceMustBeTheMonitoredPostgresPrimary(t *testing.T) {
	cases := map[string]func(*cloudSQLFake, *clone.CloudSQLCloneConfig){
		"host not the instance's IP": func(_ *cloudSQLFake, c *clone.CloudSQLCloneConfig) {
			c.Production = func(context.Context) (clone.CloudSQLProduction, error) {
				return clone.CloudSQLProduction{DSN: "postgres://app@127.0.0.1:5432/shop"}, nil
			}
		},
		"read replica": func(f *cloudSQLFake, _ *clone.CloudSQLCloneConfig) {
			f.instances[sqlSource].Type = "READ_REPLICA_INSTANCE"
		},
		"mysql": func(f *cloudSQLFake, _ *clone.CloudSQLCloneConfig) {
			f.instances[sqlSource].Version = "MYSQL_8_0"
		},
		"missing": func(_ *cloudSQLFake, c *clone.CloudSQLCloneConfig) {
			c.Instance = "gone"
		},
	}
	for name, edit := range cases {
		f, base := newCloudSQLFake(t)
		_, err := createSQL(t, base, func(c *clone.CloudSQLCloneConfig) { edit(f, c) })
		if !errors.Is(err, clone.ErrNotCloudSQLInstance) || len(f.mutating()) != 0 {
			t.Errorf("%s: err = %v, mutating %q", name, err, f.mutating())
		}
	}
	f, base := newCloudSQLFake(t)
	f.instances[sqlSource].State = "MAINTENANCE"
	if _, err := createSQL(t, base, nil); !errors.Is(err, clone.ErrCloneUnavailable) ||
		len(f.mutating()) != 0 {
		t.Fatalf("source in maintenance: err = %v", err)
	}
}

func TestCloudSQL_ErrorsNeverQuoteTheDSNOrToken(t *testing.T) {
	f, base := newCloudSQLFake(t)
	f.forbid["clone"] = true
	_, err := createSQL(t, base, nil)
	if err == nil || strings.Contains(err.Error(), sqlPassword) ||
		strings.Contains(err.Error(), sqlToken) {
		t.Fatalf("err = %v", err)
	}
}

func TestCloudSQL_DestroyDeletesOnlyPgSageClones(t *testing.T) {
	f, base := newCloudSQLFake(t)
	p := newCloudSQLProvider(t, base, nil)
	other := "sage-rehearsal-0000cccc"
	f.put(fakeSQLInstance{Name: other, State: "RUNNABLE",
		Labels: map[string]string{clone.CloudSQLReceiptLabel: "sage-rehearsal-0000dddd"}})
	guarded := "sage-rehearsal-0000eeee"
	f.put(fakeSQLInstance{Name: guarded, State: "RUNNABLE", Protected: true,
		Labels: map[string]string{clone.CloudSQLReceiptLabel: guarded}})
	for _, id := range []string{sqlSource, other, guarded, "", "sage-Rehearsal-0000eeee"} {
		if err := p.Destroy(context.Background(), clone.Clone{ID: id}); err == nil {
			t.Errorf("destroy %q succeeded", id)
		}
	}
	if len(f.mutating()) != 0 || !f.exists(other) || !f.exists(guarded) {
		t.Fatalf("mutating calls %q", f.mutating())
	}
	if err := p.Destroy(context.Background(), clone.Clone{ID: sqlName}); err != nil {
		t.Fatalf("destroy of a gone clone: %v", err)
	}
	f.put(fakeSQLInstance{Name: sqlName, State: "PENDING_DELETE",
		Labels: map[string]string{clone.CloudSQLReceiptLabel: sqlName}})
	if err := p.Destroy(context.Background(), clone.Clone{ID: sqlName}); err != nil ||
		len(f.mutating()) != 0 {
		t.Fatalf("destroy while deleting: %v, mutating %q", err, f.mutating())
	}
}

// State transitions of a lookup: labelled, unlabelled (labelling never
// finished), labelled for another receipt, and gone.
func TestCloudSQL_FindByName(t *testing.T) {
	f, base := newCloudSQLFake(t)
	api := newCloudSQLAPI(t, base, nil)
	f.put(fakeSQLInstance{Name: "sage-rehearsal-0000aaaa", State: "RUNNABLE",
		Labels: map[string]string{clone.CloudSQLReceiptLabel: "sage-rehearsal-0000aaaa"}})
	f.put(fakeSQLInstance{Name: "sage-rehearsal-0000bbbb", State: "PENDING_CREATE",
		Labels: map[string]string{"team": "shop"}})
	f.put(fakeSQLInstance{Name: "sage-rehearsal-0000cccc", State: "RUNNABLE",
		Labels: map[string]string{clone.CloudSQLReceiptLabel: "sage-rehearsal-0000dddd"}})
	want := map[string]bool{"sage-rehearsal-0000aaaa": true, "sage-rehearsal-0000bbbb": true,
		"sage-rehearsal-0000cccc": false, "sage-rehearsal-0000ffff": false}
	for name, found := range want {
		ref, ok, err := api.FindByName(context.Background(), name)
		if err != nil || ok != found || (found && ref != name) || (!found && ref != "") {
			t.Errorf("find %s = %q, %v, %v; want found %v", name, ref, ok, err, found)
		}
	}
	if _, _, err := api.FindByName(context.Background(), "shop"); !errors.Is(err,
		clone.ErrInvalidCloudSQLConfig) {
		t.Fatalf("find shop: %v", err)
	}
	f.getStatus = 500
	if _, _, err := api.FindByName(context.Background(), sqlName); err == nil {
		t.Fatal("a failed lookup reported not found")
	}
}

// The clone operation's targetId may name the source instance the call was
// made on rather than the new one: both are this clone's operation.
func TestCloudSQL_CloneOperationNamingTheSourceIsAccepted(t *testing.T) {
	f, base := newCloudSQLFake(t)
	f.cloneOpTarget = sqlSource
	c, err := createSQL(t, base, nil)
	if err != nil || c.ID != sqlName {
		t.Fatalf("create = %+v, %v; want clone %s", c, err, sqlName)
	}
	in := f.instance(sqlName)
	if in.Labels[clone.CloudSQLReceiptLabel] != sqlName || in.Protected {
		t.Fatalf("clone labels %v, protected %v: want it labelled and unprotected",
			in.Labels, in.Protected)
	}
}

// A clone reply naming a third instance is not this clone's operation:
// the clone this request made is deleted and the error names both.
func TestCloudSQL_CloneOperationNamingAnotherInstanceDeletesTheClone(t *testing.T) {
	f, base := newCloudSQLFake(t)
	f.cloneOpTarget = "billing"
	_, err := createSQL(t, base, nil)
	if err == nil || !strings.Contains(err.Error(), `"billing"`) ||
		!strings.Contains(err.Error(), sqlName) {
		t.Fatalf("err = %v, want the mismatched operation named", err)
	}
	if f.exists(sqlName) || !strings.Contains(strings.Join(f.mutating(), "|"),
		"delete "+sqlName) {
		t.Fatalf("mutating calls %q, exists %v: want the clone deleted", f.mutating(),
			f.exists(sqlName))
	}
}

// A 409 on delete is another operation in progress on the clone, never
// "already exists": the error keeps the status and nothing is deleted.
func TestCloudSQL_ConflictOnDeleteIsAnOperationInProgress(t *testing.T) {
	f, base := newCloudSQLFake(t)
	f.put(fakeSQLInstance{Name: sqlName, State: "RUNNABLE",
		Labels: map[string]string{clone.CloudSQLReceiptLabel: sqlName}})
	f.deleteStatus = 409
	err := newCloudSQLAPI(t, base, nil).Destroy(context.Background(), sqlName)
	var status *clone.ProviderStatusError
	if !errors.As(err, &status) || status.StatusCode != 409 ||
		errors.Is(err, clone.ErrCloudSQLAlreadyExists) ||
		!strings.Contains(err.Error(), "another operation") {
		t.Fatalf("err = %v, want a 409 operation-in-progress status error", err)
	}
	if !f.exists(sqlName) {
		t.Fatal("the clone was removed despite the refused delete")
	}
}
