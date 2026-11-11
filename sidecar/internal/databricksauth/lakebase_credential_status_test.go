package databricksauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/databricks/databricks-sdk-go/apierr"
)

func findCredential(t *testing.T, endpoint string) EndpointCredential {
	t.Helper()
	for _, ep := range CredentialStatuses() {
		if ep.Endpoint == endpoint {
			return ep
		}
	}
	t.Fatalf("endpoint %s missing from CredentialStatuses", endpoint)
	return EndpointCredential{}
}

// Happy path: a tracked mint is listed with its expiry and never its token.
func TestCredentialStatusesListsTrackedMintWithoutToken(t *testing.T) {
	shrinkTimings(t, time.Second)
	const endpoint = "projects/p/branches/b/endpoints/status-listed"
	f := &fakeMint{}
	seededPool(t, f, endpoint, time.Hour)
	ep := findCredential(t, endpoint)
	if time.Until(ep.State.ExpiresAt) < 59*time.Minute || ep.State.LastMintAt.IsZero() ||
		ep.State.ConsecutiveFailures != 0 || ep.State.LastErrorClass != "" {
		t.Fatalf("listed state = %+v; want a fresh mint and no failure", ep.State)
	}
	body, err := json.Marshal(CredentialStatuses())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "cached-test-token") {
		t.Fatalf("credential statuses carry the token: %s", body)
	}
}

// Nil/empty: an untracked pool (endpoint "") is never listed.
func TestCredentialStatusesSkipsUntrackedPools(t *testing.T) {
	shrinkTimings(t, time.Second)
	before := len(CredentialStatuses())
	seededPool(t, &fakeMint{}, "", time.Hour)
	if got := len(CredentialStatuses()); got != before {
		t.Fatalf("untracked pool changed the list from %d to %d", before, got)
	}
}

// Boundary: the list is sorted by endpoint so the status page is stable.
func TestCredentialStatusesAreSortedByEndpoint(t *testing.T) {
	shrinkTimings(t, time.Second)
	for _, name := range []string{"zz-sorted", "aa-sorted", "mm-sorted"} {
		seededPool(t, &fakeMint{}, "projects/p/branches/b/endpoints/"+name, time.Hour)
	}
	list := CredentialStatuses()
	for i := 1; i < len(list); i++ {
		if list[i-1].Endpoint >= list[i].Endpoint {
			t.Fatalf("not sorted at %d: %q then %q", i, list[i-1].Endpoint, list[i].Endpoint)
		}
	}
}

// Error propagation: each mint failure is reduced to a class, the failure
// count grows, and the previous good expiry stays visible.
func TestCredentialFailureClassNamesTheCauseOnly(t *testing.T) {
	for _, tc := range []struct {
		err   error
		class string
	}{
		{fmt.Errorf("secret-detail: %w", apierr.ErrPermissionDenied), "permission_denied"},
		{fmt.Errorf("secret-detail: %w", apierr.ErrUnauthenticated), "unauthenticated"},
		{fmt.Errorf("secret-detail: %w", apierr.ErrNotFound), "not_found"},
		{fmt.Errorf("secret-detail: %w", apierr.ErrTooManyRequests), "throttled"},
		{fmt.Errorf("secret-detail: %w", apierr.ErrTemporarilyUnavailable), "unavailable"},
		{fmt.Errorf("secret-detail: %w", context.DeadlineExceeded), "timeout"},
		{errBadCredential, "bad_credential"},
		{errors.New("secret-detail unknown"), "mint_error"},
	} {
		t.Run(tc.class, func(t *testing.T) {
			shrinkTimings(t, time.Second)
			endpoint := "projects/p/branches/b/endpoints/class-" + tc.class
			f := &fakeMint{}
			cfg := seededPool(t, f, endpoint, 90*time.Second)
			good := findCredential(t, endpoint).State.ExpiresAt
			f.set(fail(tc.err))
			if _, err := connectPassword(cfg); err != nil {
				t.Fatalf("connect with a usable cached token = %v", err)
			}
			_, _ = connectPassword(cfg)
			ep := findCredential(t, endpoint)
			if ep.State.LastErrorClass != tc.class || ep.State.ConsecutiveFailures != 2 {
				t.Fatalf("state = %+v; want class %q after 2 failures", ep.State, tc.class)
			}
			if !ep.State.ExpiresAt.Equal(good) {
				t.Fatalf("failure moved the expiry from %v to %v", good, ep.State.ExpiresAt)
			}
		})
	}
}

// State transition: a good mint after failures clears the class and count.
func TestCredentialRecoveryClearsTheFailureClass(t *testing.T) {
	shrinkTimings(t, time.Second)
	const endpoint = "projects/p/branches/b/endpoints/class-recovery"
	f := &fakeMint{}
	cfg := seededPool(t, f, endpoint, 90*time.Second)
	f.set(fail(apierr.ErrTooManyRequests))
	_, _ = connectPassword(cfg)
	if ep := findCredential(t, endpoint); ep.State.LastErrorClass != "throttled" {
		t.Fatalf("class after failure = %q", ep.State.LastErrorClass)
	}
	f.set(issue("fresh-test-token", time.Hour))
	if _, err := connectPassword(cfg); err != nil {
		t.Fatal(err)
	}
	ep := findCredential(t, endpoint)
	if ep.State.LastErrorClass != "" || ep.State.ConsecutiveFailures != 0 {
		t.Fatalf("state after recovery = %+v; want the failure cleared", ep.State)
	}
}
