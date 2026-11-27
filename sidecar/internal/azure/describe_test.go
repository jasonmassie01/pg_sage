package azure

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/managedparam"
)

// advisors-03: Describe and TargetFor read server parameters for
// managed-change proposals. They only ever GET: pg_sage writes no server
// parameter for a proposal.

func TestDescribeReadsOneParameter(t *testing.T) {
	arm := newFakeARM()
	arm.params["work_mem"].Source = "user-override"
	got, err := testAdapter(t, arm).Describe(context.Background(), "work_mem")
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	want := Parameter{Name: "work_mem", Value: "4096", Default: "4096", Unit: "KB",
		Source: "user-override", Dynamic: true}
	if got != want {
		t.Fatalf("Describe = %+v, want %+v", got, want)
	}
	ro, err := testAdapter(t, arm).Describe(context.Background(), "log_timezone")
	if err != nil || !ro.ReadOnly || ro.Dynamic {
		t.Fatalf("log_timezone = %+v, %v; want read-only", ro, err)
	}
	if len(arm.putBodies) != 0 {
		t.Fatalf("Describe wrote: %v", arm.putBodies)
	}
}

func TestDescribeRefusesInvalidNamesWithoutARequest(t *testing.T) {
	arm := newFakeARM()
	adapter := testAdapter(t, arm)
	for _, name := range []string{"", "Work_Mem", "work_mem/../x", "a b", "x?api-version=1"} {
		if _, err := adapter.Describe(context.Background(), name); err == nil ||
			!strings.Contains(err.Error(), "invalid parameter name") {
			t.Errorf("Describe(%q) err = %v", name, err)
		}
	}
	if len(arm.authHeaders) != 0 {
		t.Fatalf("invalid names reached ARM %d times", len(arm.authHeaders))
	}
}

// statusARM answers every request with one status and body.
func statusARM(t *testing.T, status int, body string) *ParameterAdapter {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, body, status)
	}))
	t.Cleanup(srv.Close)
	adapter, err := NewParameterAdapter(
		Server{SubscriptionID: "sub-1", ResourceGroup: "rg-1", Name: "srv-1"},
		func(context.Context) (string, error) { return "tok", nil }, WithBaseURL(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	return adapter
}

// A 403 names the permission to grant, even when ARM's message does not.
func TestDescribeForbiddenNamesConfigurationsRead(t *testing.T) {
	adapter := statusARM(t, http.StatusForbidden,
		`{"error":{"code":"AuthorizationFailed","message":"denied"}}`)
	_, err := adapter.Describe(context.Background(), "work_mem")
	if err == nil || !strings.Contains(err.Error(), "403") ||
		!strings.Contains(err.Error(), "flexibleServers/configurations/read") ||
		!strings.Contains(err.Error(), "srv-1") {
		t.Fatalf("err = %v", err)
	}
	_, err = adapter.TargetFor(context.Background(), []string{"work_mem"})
	if err == nil || !strings.Contains(err.Error(), "configurations/read") {
		t.Fatalf("TargetFor err = %v", err)
	}
	// Another failure keeps ARM's own message and gets no permission hint.
	_, err = statusARM(t, http.StatusInternalServerError,
		`{"error":{"code":"InternalServerError","message":"try later"}}`).
		Describe(context.Background(), "work_mem")
	if err == nil || !strings.Contains(err.Error(), "try later") ||
		strings.Contains(err.Error(), "configurations/read") {
		t.Fatalf("500 err = %v", err)
	}
}

func TestTargetForMapsParametersAndSkipsUnknownOnes(t *testing.T) {
	arm := newFakeARM()
	arm.params["work_mem"].Source = "user-override"
	got, err := testAdapter(t, arm).TargetFor(context.Background(),
		[]string{"shared_buffers", "work_mem", "log_timezone", "no_such_param"})
	if err != nil {
		t.Fatalf("TargetFor: %v", err)
	}
	if got.Provider != "azure" || !got.Known || got.Subscription != "sub-1" ||
		got.ResourceGroup != "rg-1" || got.InstanceID != "srv-1" || len(got.Params) != 3 {
		t.Fatalf("target = %+v", got)
	}
	want := map[string]managedparam.GroupParam{
		"shared_buffers": {Value: "16384", Default: "16384", Source: "system", Unit: "8KB",
			ApplyType: "static", Modifiable: true},
		"work_mem": {Value: "4096", Default: "4096", Source: "user", Unit: "KB",
			ApplyType: "dynamic", Modifiable: true},
		"log_timezone": {Value: "UTC", Default: "UTC", Source: "system",
			ApplyType: "static", Modifiable: false},
	}
	for name, w := range want {
		if got.Params[name] != w {
			t.Errorf("%s = %+v, want %+v", name, got.Params[name], w)
		}
	}
	if _, ok := got.Params["no_such_param"]; ok {
		t.Fatal("a parameter ARM does not have must stay undescribed")
	}
	empty, err := testAdapter(t, arm).TargetFor(context.Background(), nil)
	if err != nil || !empty.Known || len(empty.Params) != 0 {
		t.Fatalf("no names: %+v, %v", empty, err)
	}
	if len(arm.putBodies) != 0 {
		t.Fatalf("TargetFor wrote: %v", arm.putBodies)
	}
}

// The resolver and the builder agree: a described shared_buffers becomes
// a proposal in Azure's 8KB unit that needs a restart.
func TestTargetForFeedsAProposal(t *testing.T) {
	arm := newFakeARM()
	adapter := testAdapter(t, arm)
	target, err := adapter.Target(context.Background())
	if err != nil || !target.Known || len(target.Params) != 0 {
		t.Fatalf("Target = %+v, %v", target, err)
	}
	target, err = adapter.TargetFor(context.Background(), []string{"shared_buffers"})
	if err != nil {
		t.Fatal(err)
	}
	in, err := managedparam.NewIntent("azure", "shared_buffers", "4GB", "restart")
	if err != nil {
		t.Fatal(err)
	}
	p, err := managedparam.Build(in, target, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if p.Value != "524288" || !p.RebootRequired || p.Rollback.Value != "16384" ||
		!p.Rollback.Reset || !strings.Contains(p.CLI, "--server-name srv-1") ||
		p.AutoApply || len(p.Blockers) != 0 {
		t.Fatalf("proposal = %+v", p)
	}
}

func TestTargetForPropagatesTokenAndContextErrors(t *testing.T) {
	adapter, err := NewParameterAdapter(
		Server{SubscriptionID: "sub-1", ResourceGroup: "rg-1", Name: "srv-1"},
		func(context.Context) (string, error) { return "", fmt.Errorf("no credential") })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.TargetFor(context.Background(),
		[]string{"work_mem"}); err == nil || !strings.Contains(err.Error(), "no credential") {
		t.Fatalf("token failure: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = testAdapter(t, newFakeARM()).TargetFor(ctx, []string{"work_mem"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
}

// Concurrent cycles of fleet databases may describe one server at once.
func TestTargetForConcurrentReadsAgree(t *testing.T) {
	adapter := testAdapter(t, newFakeARM())
	var wg sync.WaitGroup
	errs := make(chan string, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := adapter.TargetFor(context.Background(),
				[]string{"shared_buffers", "work_mem"})
			if err != nil || got.Params["shared_buffers"].Value != "16384" ||
				got.Params["work_mem"].Unit != "KB" {
				errs <- fmt.Sprintf("%+v %v", got, err)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}
}
