package clone_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/clone"
)

// lab-05b: every way a Neon create can fail leaves no branch behind
// unrecorded, says which setting or field is wrong, and never shows the
// clone's password.

func assertNoPassword(t *testing.T, err error) {
	t.Helper()
	if err != nil && strings.Contains(err.Error(), neonFakePassword) {
		t.Fatalf("error shows the clone password: %v", err)
	}
}

func TestNeonCreate_429IsThrottledAndBacksOff(t *testing.T) {
	f := newNeonFake(t)
	f.createStatus = http.StatusTooManyRequests
	p := newNeonProvider(t, f, nil)
	_, err := p.Create(context.Background(), neonSpec(neonName))
	var status *clone.ProviderStatusError
	if !clone.Throttled(err) || !errors.As(err, &status) || status.StatusCode != 429 {
		t.Fatalf("err = %v, want a throttled 429 ProviderStatusError", err)
	}
	backoff := clone.NewCreateBackoff(func() time.Time { return neonNow })
	backoff.Observe(err)
	if err := backoff.Allow(); !errors.Is(err, clone.ErrProviderBackoff) {
		t.Fatalf("after a 429 the next create is allowed: %v", err)
	}
	if _, ok := f.branchNamed(neonName); ok {
		t.Fatal("a refused create left a branch")
	}
}

func TestNeonCreate_401NamesTheKeySetting(t *testing.T) {
	f := newNeonFake(t)
	p := newNeonProvider(t, f, func(c *clone.NeonConfig) { c.APIKey = "napi_wrong" })
	_, err := p.Create(context.Background(), neonSpec(neonName))
	var status *clone.ProviderStatusError
	if !errors.As(err, &status) || status.StatusCode != http.StatusUnauthorized ||
		!strings.Contains(err.Error(), "clone.neon.api_key") || clone.Throttled(err) {
		t.Fatalf("err = %v, want a 401 naming clone.neon.api_key", err)
	}
	if strings.Contains(err.Error(), "napi_wrong") {
		t.Fatalf("error shows the API key: %v", err)
	}
}

func TestNeonCreate_EndpointFailureDeletesTheBranch(t *testing.T) {
	f := newNeonFake(t)
	f.failAction = "start_compute"
	p := newNeonProvider(t, f, nil)
	_, err := p.Create(context.Background(), neonSpec(neonName))
	if err == nil || !strings.Contains(err.Error(), "start_compute") ||
		!strings.Contains(err.Error(), "failed") {
		t.Fatalf("err = %v, want the failed start_compute operation named", err)
	}
	if errors.Is(err, clone.ErrCloneLeftBehind) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a cleaned-up create is reported uncertain: %v", err)
	}
	if _, ok := f.branchNamed(neonName); ok || f.countRequests("DELETE ") != 1 {
		t.Fatalf("branch left after the endpoint failed: %v", f.requestLog())
	}
}

func TestNeonCreate_CleanupFailureIsLeftBehind(t *testing.T) {
	f := newNeonFake(t)
	f.failAction, f.deleteStatus = "start_compute", http.StatusLocked
	p := newNeonProvider(t, f, nil)
	_, err := p.Create(context.Background(), neonSpec(neonName))
	if !errors.Is(err, clone.ErrCloneLeftBehind) || !strings.Contains(err.Error(), neonName) {
		t.Fatalf("err = %v, want ErrCloneLeftBehind naming the branch", err)
	}
}

func TestNeonCreate_EndpointOnProductionHostIsRefusedAndDeleted(t *testing.T) {
	f := newNeonFake(t)
	f.endpointHost = neonProdHost
	p := newNeonProvider(t, f, nil)
	_, err := p.Create(context.Background(), neonSpec(neonName))
	if !errors.Is(err, clone.ErrCloneIsProduction) {
		t.Fatalf("err = %v, want ErrCloneIsProduction", err)
	}
	if _, ok := f.branchNamed(neonName); ok {
		t.Fatal("the branch whose endpoint answered on production's host was kept")
	}
	if !f.exists(neonProdBranch) {
		t.Fatal("production's branch was deleted")
	}
}

func TestNeonCreate_NoConnectionURINamesTheFieldAndDeletesTheBranch(t *testing.T) {
	f := newNeonFake(t)
	f.noURI = true
	p := newNeonProvider(t, f, nil)
	_, err := p.Create(context.Background(), neonSpec(neonName))
	if err == nil || !strings.Contains(err.Error(), `"uri"`) {
		t.Fatalf("err = %v, want the missing uri field named", err)
	}
	if _, ok := f.branchNamed(neonName); ok {
		t.Fatal("branch left after the connection URI was missing")
	}
}

func TestNeonCreate_PollPastTheDeadlineIsUncertainAndFoundByName(t *testing.T) {
	f := newNeonFake(t)
	f.opsNeverFinish = true
	p := newNeonProvider(t, f, func(c *clone.NeonConfig) { c.WaitTimeout = 50 * time.Millisecond })
	_, err := p.Create(context.Background(), neonSpec(neonName))
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), neonName) {
		t.Fatalf("err = %v, want a deadline naming the branch (create_uncertain)", err)
	}
	b, ok := f.branchNamed(neonName)
	if !ok {
		t.Fatal("the branch still being made was deleted; the reaper resolves it by name")
	}
	ref, found, err := p.FindByName(context.Background(), neonName)
	if err != nil || !found || ref != b.ID {
		t.Fatalf("find = %q, %v, %v: want %s", ref, found, err, b.ID)
	}
	if err := p.Destroy(context.Background(), clone.Clone{ID: ref}); err != nil || f.exists(ref) {
		t.Fatalf("reaping the found branch: %v (exists %v)", err, f.exists(ref))
	}
}

func TestNeonCreate_CallerCancelIsUncertain(t *testing.T) {
	f := newNeonFake(t)
	f.opsNeverFinish = true
	p := newNeonProvider(t, f, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := p.Create(ctx, neonSpec(neonName))
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the caller's context error", err)
	}
}

func TestNeonCreate_PasswordNeverInErrors(t *testing.T) {
	for _, knob := range []func(*neonFake){
		func(f *neonFake) { f.endpointHost = neonProdHost },
		func(f *neonFake) { f.failAction = "start_compute" },
		func(f *neonFake) { f.noURI = true },
	} {
		f := newNeonFake(t)
		knob(f)
		_, err := newNeonProvider(t, f, nil).Create(context.Background(), neonSpec(neonName))
		if err == nil {
			t.Fatal("create succeeded under an injected failure")
		}
		assertNoPassword(t, err)
	}
}

func TestNeonDestroy_RefusesBranchesItDidNotMake(t *testing.T) {
	f := newNeonFake(t)
	p := newNeonProvider(t, f, nil)
	for _, id := range []string{neonProdBranch, neonDevBranch} {
		err := p.Destroy(context.Background(), clone.Clone{ID: id})
		if !errors.Is(err, clone.ErrInvalidNeonConfig) || !f.exists(id) {
			t.Fatalf("destroy %s: %v (exists %v), want refused", id, err, f.exists(id))
		}
	}
	for _, id := range []string{"", "main", "br-../x", "BR-1"} {
		if err := p.Destroy(context.Background(), clone.Clone{ID: id}); !errors.Is(err,
			clone.ErrInvalidNeonConfig) {
			t.Fatalf("destroy %q: %v, want refused", id, err)
		}
	}
	if n := f.countRequests("DELETE "); n != 0 {
		t.Fatalf("%d deletes sent for branches pg_sage did not make", n)
	}
}

func TestNeonDestroy_AGoneBranchIsDestroyed(t *testing.T) {
	f := newNeonFake(t)
	p := newNeonProvider(t, f, nil)
	if err := p.Destroy(context.Background(), clone.Clone{ID: "br-fake-99"}); err != nil {
		t.Fatalf("destroy of a branch already gone: %v", err)
	}
}

func TestNeonDestroy_ErrorPropagates(t *testing.T) {
	f := newNeonFake(t)
	p := newNeonProvider(t, f, nil)
	c, err := p.Create(context.Background(), neonSpec(neonName))
	if err != nil {
		t.Fatal(err)
	}
	f.deleteStatus = http.StatusLocked
	err = p.Destroy(context.Background(), c)
	var status *clone.ProviderStatusError
	if !errors.As(err, &status) || status.StatusCode != http.StatusLocked ||
		!strings.Contains(err.Error(), c.ID) || !f.exists(c.ID) {
		t.Fatalf("err = %v, want the 423 naming %s", err, c.ID)
	}
}

func TestNeonFindByName(t *testing.T) {
	f := newNeonFake(t)
	p := newNeonProvider(t, f, nil)
	if _, found, err := p.FindByName(context.Background(), neonName); err != nil || found {
		t.Fatalf("before create: found %v, %v", found, err)
	}
	c, err := p.Create(context.Background(), neonSpec(neonName))
	if err != nil {
		t.Fatal(err)
	}
	// Another rehearsal branch in the project is not this one.
	other := neonSpec(neonName[:len(neonName)-1] + "e")
	if _, err := p.Create(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	ref, found, err := p.FindByName(context.Background(), neonName)
	if err != nil || !found || ref != c.ID {
		t.Fatalf("find = %q, %v, %v: want %s", ref, found, err, c.ID)
	}
	if _, _, err := p.FindByName(context.Background(), "main"); !errors.Is(err,
		clone.ErrInvalidNeonConfig) {
		t.Fatalf("find of a name pg_sage never gives: %v", err)
	}
}
