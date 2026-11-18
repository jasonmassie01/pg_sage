package clone_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/clone"
)

// lab-05c error propagation: every failure after the cluster exists
// deletes it before Create returns, or says it was left behind.

func TestAuroraCreate_InstanceFailureDeletesTheCluster(t *testing.T) {
	f := newAuroraFake(t)
	f.createInstanceErr = &clone.ProviderStatusError{Op: "fake create instance",
		StatusCode: 400}
	p := newAuroraProvider(t, f, nil)
	_, err := p.Create(context.Background(), clone.CloneSpec{ResourceName: auroraName})
	if err == nil || !strings.Contains(err.Error(), "create instance "+auroraName+"-1") {
		t.Fatalf("err = %v, want the instance failure", err)
	}
	if errors.Is(err, clone.ErrCloneLeftBehind) || f.exists(auroraName) {
		t.Fatalf("err = %v, cluster exists %v: want it deleted before returning", err,
			f.exists(auroraName))
	}
	if got := f.callsWith("delete-cluster"); len(got) != 1 {
		t.Fatalf("delete-cluster calls = %q", got)
	}
}

func TestAuroraCreate_CleanupFailureIsLeftBehind(t *testing.T) {
	f := newAuroraFake(t)
	f.createInstanceErr = errors.New("instance quota exceeded")
	f.deleteClusterErr = errors.New("delete refused")
	p := newAuroraProvider(t, f, nil)
	_, err := p.Create(context.Background(), clone.CloneSpec{ResourceName: auroraName})
	if !errors.Is(err, clone.ErrCloneLeftBehind) || !strings.Contains(err.Error(),
		"delete refused") || !strings.Contains(err.Error(), "instance quota exceeded") {
		t.Fatalf("err = %v, want both causes and ErrCloneLeftBehind", err)
	}
}

func TestAuroraCreate_InstanceEndingFailedDeletesBoth(t *testing.T) {
	f := newAuroraFake(t)
	f.instanceEndStatus = "failed"
	p := newAuroraProvider(t, f, nil)
	_, err := p.Create(context.Background(), clone.CloneSpec{ResourceName: auroraName})
	if err == nil || !strings.Contains(err.Error(), "failed") {
		t.Fatalf("err = %v", err)
	}
	if f.exists(auroraName) || f.instanceExists(auroraName+"-1") {
		t.Fatal("a failed clone was left")
	}
}

func TestAuroraCreate_RefusedRestoreLooksForNothing(t *testing.T) {
	f := newAuroraFake(t)
	f.restoreErr = &clone.ProviderStatusError{Op: "fake restore", StatusCode: 403}
	p := newAuroraProvider(t, f, nil)
	_, err := p.Create(context.Background(), clone.CloneSpec{ResourceName: auroraName})
	var status *clone.ProviderStatusError
	if !errors.As(err, &status) || status.StatusCode != 403 || clone.Throttled(err) {
		t.Fatalf("err = %v, want the 403", err)
	}
	if got := f.mutating(); len(got) != 1 {
		t.Fatalf("calls after a refused restore = %q, want the restore only", got)
	}
}

func TestAuroraCreate_ThrottledRestoreIsThrottled(t *testing.T) {
	f := newAuroraFake(t)
	f.restoreErr = &clone.ProviderStatusError{Op: "fake restore", StatusCode: 429}
	p := newAuroraProvider(t, f, nil)
	_, err := p.Create(context.Background(), clone.CloneSpec{ResourceName: auroraName})
	if !clone.Throttled(err) || errors.Is(err, clone.ErrCloneLeftBehind) {
		t.Fatalf("err = %v, want a throttled error with nothing left", err)
	}
}

func TestAuroraCreate_5xxAfterTheClusterWasMadeDeletesItByTag(t *testing.T) {
	f := newAuroraFake(t)
	f.restoreThenErr = &clone.ProviderStatusError{Op: "fake restore", StatusCode: 503}
	p := newAuroraProvider(t, f, nil)
	_, err := p.Create(context.Background(), clone.CloneSpec{ResourceName: auroraName})
	if !clone.Throttled(err) || errors.Is(err, clone.ErrCloneLeftBehind) {
		t.Fatalf("err = %v", err)
	}
	if f.exists(auroraName) {
		t.Fatal("the cluster a lost reply made was not found and deleted")
	}
}

func TestAuroraCreate_WaitPastTheDeadlineIsUncertainAndFoundByTag(t *testing.T) {
	f := newAuroraFake(t)
	f.neverReady = true
	p := newAuroraProvider(t, f, func(c *clone.AuroraConfig) {
		c.WaitTimeout = 30 * time.Millisecond
	})
	_, err := p.Create(context.Background(), clone.CloneSpec{ResourceName: auroraName})
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(),
		"reaper") {
		t.Fatalf("err = %v, want the wait deadline and the reaper named", err)
	}
	if !f.exists(auroraName) || len(f.callsWith("delete-")) != 0 {
		t.Fatal("a clone still creating was deleted instead of left to the reaper")
	}
	ref, found, err := p.FindByName(context.Background(), auroraName)
	if err != nil || !found || ref != auroraName {
		t.Fatalf("find = %q %v %v", ref, found, err)
	}
}

func TestAuroraCreate_CallerCancelIsUncertain(t *testing.T) {
	f := newAuroraFake(t)
	f.neverReady = true
	p := newAuroraProvider(t, f, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := p.Create(ctx, clone.CloneSpec{ResourceName: auroraName})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
}

func TestAuroraCreate_CloneOnProductionHostIsRefusedAndDeleted(t *testing.T) {
	f := newAuroraFake(t)
	f.cloneEndpoint = auroraProdHost
	p := newAuroraProvider(t, f, nil)
	_, err := p.Create(context.Background(), clone.CloneSpec{ResourceName: auroraName})
	if !errors.Is(err, clone.ErrCloneIsProduction) || f.exists(auroraName) {
		t.Fatalf("err = %v, exists %v", err, f.exists(auroraName))
	}
}

func TestAuroraCreate_PasswordNeverInErrors(t *testing.T) {
	f := newAuroraFake(t)
	f.cloneEndpoint = auroraProdHost
	p := newAuroraProvider(t, f, nil)
	_, err := p.Create(context.Background(), clone.CloneSpec{ResourceName: auroraName})
	if err == nil || strings.Contains(err.Error(), auroraPassword) {
		t.Fatalf("err = %v", err)
	}
}

func TestAuroraDestroy_RefusesClustersItDidNotMake(t *testing.T) {
	f := newAuroraFake(t)
	f.clusters["sage-rehearsal-99999999"] = &clone.AuroraCluster{ID: "sage-rehearsal-99999999",
		Engine: "aurora-postgresql", Status: "available"}
	f.clusters["sage-rehearsal-88888888"] = &clone.AuroraCluster{ID: "sage-rehearsal-88888888",
		Status: "available", DeletionProtection: true,
		Tags: map[string]string{clone.AuroraReceiptTag: "sage-rehearsal-88888888"}}
	p := newAuroraProvider(t, f, nil)
	for _, id := range []string{"shop", "", "sage-rehearsal-99999999",
		"sage-rehearsal-88888888"} {
		err := p.Destroy(context.Background(), clone.Clone{ID: id})
		if !errors.Is(err, clone.ErrInvalidAuroraConfig) {
			t.Errorf("destroy %q: err = %v, want refused", id, err)
		}
	}
	if got := f.callsWith("delete-"); len(got) != 0 {
		t.Fatalf("deletes = %q", got)
	}
}

func TestAuroraDestroy_AGoneOrDeletingClusterIsDestroyed(t *testing.T) {
	f := newAuroraFake(t)
	p := newAuroraProvider(t, f, nil)
	if err := p.Destroy(context.Background(), clone.Clone{ID: auroraName}); err != nil {
		t.Fatalf("gone: %v", err)
	}
	f.clusters[auroraName] = &clone.AuroraCluster{ID: auroraName, Status: "deleting",
		Tags: map[string]string{clone.AuroraReceiptTag: auroraName}}
	if err := p.Destroy(context.Background(), clone.Clone{ID: auroraName}); err != nil {
		t.Fatalf("deleting: %v", err)
	}
	if got := f.callsWith("delete-"); len(got) != 0 {
		t.Fatalf("deletes = %q, want none", got)
	}
}

func TestAuroraDestroy_ErrorPropagates(t *testing.T) {
	f := newAuroraFake(t)
	p := newAuroraProvider(t, f, nil)
	c, err := p.Create(context.Background(), clone.CloneSpec{ResourceName: auroraName})
	if err != nil {
		t.Fatal(err)
	}
	f.deleteClusterErr = errors.New("cluster delete throttled")
	err = p.Destroy(context.Background(), c)
	if err == nil || !strings.Contains(err.Error(), "cluster delete throttled") ||
		!strings.Contains(err.Error(), auroraName) || !f.exists(auroraName) {
		t.Fatalf("err = %v, exists %v", err, f.exists(auroraName))
	}
	f.deleteClusterErr = nil
	if err := p.Destroy(context.Background(), c); err != nil || f.exists(auroraName) {
		t.Fatalf("retry: %v, exists %v", err, f.exists(auroraName))
	}
}

func TestAuroraFindByName(t *testing.T) {
	f := newAuroraFake(t)
	f.clusters["sage-rehearsal-77777777"] = &clone.AuroraCluster{ID: "sage-rehearsal-77777777",
		Status: "available"}
	f.clusters["sage-rehearsal-66666666"] = &clone.AuroraCluster{ID: "sage-rehearsal-66666666",
		Status: "available",
		Tags:   map[string]string{clone.AuroraReceiptTag: "sage-rehearsal-66666666"}}
	p := newAuroraProvider(t, f, nil)
	cases := []struct {
		name  string
		found bool
	}{{"sage-rehearsal-66666666", true}, {"sage-rehearsal-77777777", false},
		{"sage-rehearsal-55555555", false}}
	for _, tc := range cases {
		ref, found, err := p.FindByName(context.Background(), tc.name)
		if err != nil || found != tc.found || (found && ref != tc.name) {
			t.Errorf("find %s = %q %v %v, want found %v", tc.name, ref, found, err, tc.found)
		}
	}
	if _, _, err := p.FindByName(context.Background(), "shop"); !errors.Is(err,
		clone.ErrInvalidAuroraConfig) {
		t.Fatalf("find shop err = %v", err)
	}
	f.listErr = errors.New("list failed")
	if _, _, err := p.FindByName(context.Background(),
		"sage-rehearsal-66666666"); err == nil {
		t.Fatal("a failed list was read as not found")
	}
}
