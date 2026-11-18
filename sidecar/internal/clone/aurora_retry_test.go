package clone_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/clone"
)

// lab-05c review: Aurora has no server-side expiry, so a restore whose
// outcome is unknown must never close its receipt as if nothing was made.
// The SDK retries a restore whose reply was lost (a 5xx or a reset
// connection); the retry of an accepted restore is answered
// DBClusterAlreadyExistsFault, a 400.

func alreadyExists() error {
	return fmt.Errorf("%w: aurora RestoreDBClusterToPointInTime: "+
		"DBClusterAlreadyExistsFault: DB cluster already exists: %w",
		clone.ErrAuroraAlreadyExists, &clone.ProviderStatusError{Op: "fake restore",
			StatusCode: 400})
}

func TestAuroraCreate_AlreadyExistsAfterARetryDeletesTheClusterByTag(t *testing.T) {
	f := newAuroraFake(t)
	// The first attempt RDS accepted: the cluster exists, tagged, creating.
	f.clusters[auroraName] = &clone.AuroraCluster{ID: auroraName,
		Engine: "aurora-postgresql", Status: "creating",
		Tags: map[string]string{clone.AuroraReceiptTag: auroraName}}
	f.restoreErr = alreadyExists()
	p := newAuroraProvider(t, f, nil)
	_, err := p.Create(context.Background(), clone.CloneSpec{ResourceName: auroraName})
	if !errors.Is(err, clone.ErrAuroraAlreadyExists) {
		t.Fatalf("err = %v, want the already-exists cause", err)
	}
	if errors.Is(err, clone.ErrCloneLeftBehind) {
		t.Fatalf("err = %v: the cluster was deleted, nothing is left behind", err)
	}
	if f.exists(auroraName) {
		t.Fatal("the cluster an accepted first attempt made was left with no receipt")
	}
	if got := f.callsWith("delete-cluster"); len(got) != 1 || got[0] !=
		"delete-cluster "+auroraName {
		t.Fatalf("delete-cluster calls = %q, want exactly %s", got, auroraName)
	}
}

func TestAuroraCreate_AlreadyExistsWithoutTheTagDeletesNothing(t *testing.T) {
	f := newAuroraFake(t)
	f.clusters[auroraName] = &clone.AuroraCluster{ID: auroraName,
		Engine: "aurora-postgresql", Status: "available",
		Tags: map[string]string{"team": "shop"}}
	f.restoreErr = alreadyExists()
	p := newAuroraProvider(t, f, nil)
	_, err := p.Create(context.Background(), clone.CloneSpec{ResourceName: auroraName})
	if !errors.Is(err, clone.ErrAuroraAlreadyExists) {
		t.Fatalf("err = %v, want the already-exists cause", err)
	}
	if !f.exists(auroraName) || len(f.callsWith("delete-")) != 0 {
		t.Fatalf("a cluster pg_sage did not tag was deleted: calls %q", f.callsWith("delete-"))
	}
}

func TestAuroraCreate_5xxWithNoClusterListedStaysUncertain(t *testing.T) {
	f := newAuroraFake(t)
	f.restoreErr = &clone.ProviderStatusError{Op: "fake restore", StatusCode: 503}
	p := newAuroraProvider(t, f, nil)
	_, err := p.Create(context.Background(), clone.CloneSpec{ResourceName: auroraName})
	if !errors.Is(err, clone.ErrCloneLeftBehind) || !clone.Throttled(err) {
		t.Fatalf("err = %v, want the 503 and an uncertain create for the reaper", err)
	}
	if !strings.Contains(err.Error(), auroraName) || !strings.Contains(err.Error(),
		clone.AuroraReceiptTag) {
		t.Fatalf("err = %v, want the cluster name and the tag the reaper looks for", err)
	}
	if len(f.callsWith("list-clusters")) < 2 {
		t.Fatalf("calls = %q, want the tag lookup after the count", f.calls)
	}
	if got := f.callsWith("delete-"); len(got) != 0 {
		t.Fatalf("delete calls = %q with nothing to delete", got)
	}
}

func TestAuroraCreate_RefusedRestoreStaysCertain(t *testing.T) {
	f := newAuroraFake(t)
	f.restoreErr = &clone.ProviderStatusError{Op: "fake restore", StatusCode: 400}
	p := newAuroraProvider(t, f, nil)
	_, err := p.Create(context.Background(), clone.CloneSpec{ResourceName: auroraName})
	if err == nil || errors.Is(err, clone.ErrCloneLeftBehind) ||
		errors.Is(err, clone.ErrAuroraAlreadyExists) {
		t.Fatalf("err = %v, want a plain refusal: nothing was made", err)
	}
}

func TestAuroraSDK_AlreadyExistsIsDistinguishable(t *testing.T) {
	for _, code := range []string{"DBClusterAlreadyExistsFault", "DBInstanceAlreadyExists"} {
		api := clone.NewAuroraSDKAPI(&fakeRDS{err: fakeAPIError{code, "already exists"}})
		_, err := api.RestoreCluster(context.Background(), clone.AuroraRestoreRequest{
			ID: auroraName, Source: "shop"})
		var status *clone.ProviderStatusError
		if !errors.Is(err, clone.ErrAuroraAlreadyExists) || !errors.As(err, &status) ||
			status.StatusCode != 400 || !strings.Contains(err.Error(), code) {
			t.Errorf("%s: err = %v, want ErrAuroraAlreadyExists with status 400", code, err)
		}
		if errors.Is(err, clone.ErrAuroraNotFound) {
			t.Errorf("%s: err = %v reads as not found", code, err)
		}
	}
}
