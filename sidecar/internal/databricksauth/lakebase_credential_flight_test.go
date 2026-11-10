package databricksauth

import (
	"context"
	"testing"
	"time"

	"github.com/databricks/databricks-sdk-go/service/postgres"
)

// releasedMint blocks each mint until release closes, then issues token.
func releasedMint(
	release <-chan struct{}, token string, ttl time.Duration,
) mintResponse {
	return func(context.Context, int) (*postgres.DatabaseCredential, error) {
		<-release
		return credentialFor(token, ttl), nil
	}
}

func waitForCalls(t *testing.T, f *fakeMint, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for f.callCount() < n {
		if time.Now().After(deadline) {
			t.Fatalf("mint calls = %d; want %d", f.callCount(), n)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestCredentialUsableTokenNeverWaitsForInFlightMint(t *testing.T) {
	shrinkTimings(t, 10*time.Second)
	release := make(chan struct{})
	f := &fakeMint{}
	cfg := seededPool(t, f, "", 90*time.Second)
	f.set(releasedMint(release, "fresh-test-token", time.Hour))
	leader := make(chan error, 1)
	go func() { _, err := connectPassword(cfg); leader <- err }()
	waitForCalls(t, f, 2)
	done := make(chan []string, 1)
	go func() {
		var got []string
		for range 5 {
			token, err := connectPassword(cfg)
			if err != nil {
				token = "error: " + err.Error()
			}
			got = append(got, token)
		}
		done <- got
	}()
	select {
	case got := <-done:
		for _, token := range got {
			if token != "cached-test-token" {
				t.Fatalf("connect during an in-flight mint = %q; want the cached token", token)
			}
		}
	case <-time.After(time.Second):
		close(release)
		t.Fatal("connects with a usable cached token waited for the in-flight mint")
	}
	close(release)
	if err := <-leader; err != nil {
		t.Fatalf("leader connect = %v", err)
	}
	if got, err := connectPassword(cfg); err != nil || got != "fresh-test-token" {
		t.Fatalf("connect after the mint = %q, %v; want the fresh token", got, err)
	}
	if f.callCount() != 2 {
		t.Fatalf("mint calls = %d; want seed and one refresh", f.callCount())
	}
}

func TestCredentialWaiterSharesSuccessfulShortLivedMint(t *testing.T) {
	shrinkTimings(t, 10*time.Second)
	release := make(chan struct{})
	f := &fakeMint{}
	// A successful mint below minUsable is still valid at login; the leader
	// uses it, so a waiter sharing that mint must too.
	f.set(releasedMint(release, "short-test-token", 10*time.Second))
	cfg := testPool(t, "verify-full")
	if err := configurePool(cfg, []string{"db.example"}, f.mint); err != nil {
		t.Fatal(err)
	}
	type result struct {
		token string
		err   error
	}
	leader, waiter := make(chan result, 1), make(chan result, 1)
	go func() { token, err := connectPassword(cfg); leader <- result{token, err} }()
	waitForCalls(t, f, 1)
	go func() { token, err := connectPassword(cfg); waiter <- result{token, err} }()
	time.Sleep(50 * time.Millisecond) // let the waiter join the flight
	close(release)
	for name, ch := range map[string]chan result{"leader": leader, "waiter": waiter} {
		r := <-ch
		if r.err != nil || r.token != "short-test-token" {
			t.Fatalf("%s = %q, %v; want the freshly minted token", name, r.token, r.err)
		}
	}
}
