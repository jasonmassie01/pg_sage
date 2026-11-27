package databricksauth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	sdktime "github.com/databricks/databricks-sdk-go/common/types/time"
	"github.com/databricks/databricks-sdk-go/service/postgres"
	"github.com/jackc/pgx/v5/pgxpool"
)

// fakeMint is a programmable credential service. It counts calls and the
// largest number of mints that ran at the same time.
type fakeMint struct {
	mu          sync.Mutex
	calls       int
	inFlight    int
	maxInFlight int
	respond     func(context.Context, int) (*postgres.DatabaseCredential, error)
}

func (f *fakeMint) mint(ctx context.Context) (*postgres.DatabaseCredential, error) {
	f.mu.Lock()
	f.calls++
	call := f.calls
	f.inFlight++
	f.maxInFlight = max(f.maxInFlight, f.inFlight)
	respond := f.respond
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.inFlight--
		f.mu.Unlock()
	}()
	return respond(ctx, call)
}

func (f *fakeMint) set(respond func(context.Context, int) (*postgres.DatabaseCredential, error)) {
	f.mu.Lock()
	f.respond = respond
	f.mu.Unlock()
}

func (f *fakeMint) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func credentialFor(token string, ttl time.Duration) *postgres.DatabaseCredential {
	return &postgres.DatabaseCredential{Token: token, ExpireTime: sdktime.New(time.Now().Add(ttl))}
}

type mintResponse = func(context.Context, int) (*postgres.DatabaseCredential, error)

func issue(token string, ttl time.Duration) mintResponse {
	return func(context.Context, int) (*postgres.DatabaseCredential, error) {
		return credentialFor(token, ttl), nil
	}
}

func fail(err error) func(context.Context, int) (*postgres.DatabaseCredential, error) {
	return func(context.Context, int) (*postgres.DatabaseCredential, error) { return nil, err }
}

// blockUntilDone models a control-plane brownout: the mint returns only when
// its context ends.
func blockUntilDone(ctx context.Context, _ int) (*postgres.DatabaseCredential, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// seededPool returns a pool hook whose cache already holds a token with ttl
// left, minted by f's first call. endpoint "" leaves the pool untracked.
func seededPool(t *testing.T, f *fakeMint, endpoint string, ttl time.Duration) *pgxpool.Config {
	t.Helper()
	cfg := testPool(t, "verify-full")
	f.set(issue("cached-test-token", ttl))
	var opts []poolOption
	if endpoint != "" {
		opts = append(opts, trackedAs(endpoint))
	}
	if err := configurePool(cfg, []string{"db.example"}, f.mint, opts...); err != nil {
		t.Fatal(err)
	}
	if got, err := connectPassword(cfg); err != nil || got != "cached-test-token" {
		t.Fatalf("seed connect = %q, %v", got, err)
	}
	return cfg
}

func connectPassword(cfg *pgxpool.Config) (string, error) {
	conn := cfg.ConnConfig.Copy()
	err := cfg.BeforeConnect(context.Background(), conn)
	return conn.Password, err
}

func shrinkTimings(t *testing.T, mint time.Duration) {
	t.Helper()
	oldMint, oldWarn := mintTimeout, credentialWarn
	mintTimeout = mint
	credentialWarn = func(string, ...any) {}
	t.Cleanup(func() { mintTimeout, credentialWarn = oldMint, oldWarn })
}

func TestCredentialFallbackKeepsConnectingThroughMintFailure(t *testing.T) {
	shrinkTimings(t, time.Second)
	const endpoint = "projects/p/branches/b/endpoints/fallback"
	f := &fakeMint{}
	cfg := seededPool(t, f, endpoint, 90*time.Second)
	f.set(fail(errors.New("credential service unavailable")))
	got, err := connectPassword(cfg)
	if err != nil || got != "cached-test-token" {
		t.Fatalf("connect during mint failure = %q, %v; want the cached token", got, err)
	}
	state, ok := CredentialStatus(endpoint)
	if !ok || state.ConsecutiveFailures != 1 {
		t.Fatalf("status = %+v, %v; want one consecutive failure", state, ok)
	}
	if !strings.Contains(state.LastError, "credential service unavailable") {
		t.Fatalf("LastError = %q; want the mint failure", state.LastError)
	}
	f.set(issue("fresh-test-token", time.Hour))
	got, err = connectPassword(cfg)
	if err != nil || got != "fresh-test-token" {
		t.Fatalf("connect after recovery = %q, %v; want the fresh token", got, err)
	}
	state, _ = CredentialStatus(endpoint)
	if state.ConsecutiveFailures != 0 || state.LastError != "" {
		t.Fatalf("status after a good mint = %+v; want failures reset", state)
	}
	if time.Until(state.ExpiresAt) < 59*time.Minute || time.Since(state.LastMintAt) > time.Minute {
		t.Fatalf("status times = %+v; want the fresh mint recorded", state)
	}
	if f.callCount() != 3 {
		t.Fatalf("mint calls = %d; want seed, failure, recovery", f.callCount())
	}
}

func TestCredentialBelowMinimumUsableFailsClosed(t *testing.T) {
	shrinkTimings(t, time.Second)
	f := &fakeMint{}
	cfg := seededPool(t, f, "", 29*time.Second)
	failed := errors.New("credential service unavailable")
	f.set(fail(failed))
	got, err := connectPassword(cfg)
	if !errors.Is(err, failed) ||
		!strings.Contains(err.Error(), "refresh Lakebase database credential") {
		t.Fatalf("connect with 29s left = %v; want the wrapped refresh error", err)
	}
	if got != "" {
		t.Fatal("refused connection received a credential")
	}
}

func TestCredentialRefreshWindowBoundaries(t *testing.T) {
	shrinkTimings(t, time.Second)
	cases := []struct {
		ttl       time.Duration
		wantMints int
	}{
		{16 * time.Minute, 0},
		{14 * time.Minute, 1},
	}
	for _, tc := range cases {
		t.Run(tc.ttl.String(), func(t *testing.T) {
			f := &fakeMint{}
			cfg := seededPool(t, f, "", tc.ttl)
			f.set(issue("fresh-test-token", time.Hour))
			for range 3 {
				if _, err := connectPassword(cfg); err != nil {
					t.Fatal(err)
				}
			}
			if extra := f.callCount() - 1; extra != tc.wantMints {
				t.Fatalf("mints after seeding = %d; want %d", extra, tc.wantMints)
			}
		})
	}
}

func TestCredentialMintIsBoundedByMintTimeout(t *testing.T) {
	shrinkTimings(t, 150*time.Millisecond)
	f := &fakeMint{}
	var deadline time.Duration
	f.set(func(ctx context.Context, _ int) (*postgres.DatabaseCredential, error) {
		d, ok := ctx.Deadline()
		if !ok {
			return nil, errors.New("mint context has no deadline")
		}
		deadline = time.Until(d)
		return credentialFor("bounded-test-token", time.Hour), nil
	})
	cfg := testPool(t, "verify-full")
	if err := configurePool(cfg, []string{"db.example"}, f.mint); err != nil {
		t.Fatal(err)
	}
	if _, err := connectPassword(cfg); err != nil {
		t.Fatal(err)
	}
	if deadline <= 0 || deadline > 150*time.Millisecond {
		t.Fatalf("mint deadline = %s; want at most mintTimeout", deadline)
	}
}

func TestCredentialConcurrentConnectsDoNotQueueBehindStalledMint(t *testing.T) {
	shrinkTimings(t, 200*time.Millisecond)
	f := &fakeMint{}
	cfg := seededPool(t, f, "", 90*time.Second)
	f.set(blockUntilDone)
	start := time.Now()
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			if got, err := connectPassword(cfg); err != nil || got != "cached-test-token" {
				t.Errorf("concurrent connect = %q, %v", got, err)
			}
		})
	}
	wg.Wait()
	// Serialized behind a held lock, 20 stalled mints would take 4 s.
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("20 connects took %s; want about one mint timeout", elapsed)
	}
	if f.maxInFlight != 1 {
		t.Fatalf("max mints in flight = %d; want 1", f.maxInFlight)
	}
}

func TestCredentialConcurrentFirstMintFailureIsShared(t *testing.T) {
	shrinkTimings(t, 200*time.Millisecond)
	f := &fakeMint{}
	f.set(blockUntilDone)
	cfg := testPool(t, "verify-full")
	if err := configurePool(cfg, []string{"db.example"}, f.mint); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			got, err := connectPassword(cfg)
			if !errors.Is(err, context.DeadlineExceeded) || got != "" {
				t.Errorf("connect without any token = %q, %v; want the mint timeout", got, err)
			}
		})
	}
	wg.Wait()
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("20 connects took %s; want one shared mint timeout", elapsed)
	}
	if f.callCount() != 1 || f.maxInFlight != 1 {
		t.Fatalf("mint calls = %d, max in flight = %d; want one shared mint",
			f.callCount(), f.maxInFlight)
	}
}

func TestCredentialEmptyMintedCredentialIsAFailure(t *testing.T) {
	shrinkTimings(t, time.Second)
	bad := []*postgres.DatabaseCredential{
		nil,
		{Token: "", ExpireTime: sdktime.New(time.Now().Add(time.Hour))},
		{Token: "no-expiry-test-token"},
		credentialFor("expired-test-token", -time.Minute),
	}
	for i, cred := range bad {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			f := &fakeMint{}
			cfg := seededPool(t, f, "", 90*time.Second)
			f.set(func(context.Context, int) (*postgres.DatabaseCredential, error) {
				return cred, nil
			})
			if got, err := connectPassword(cfg); err != nil || got != "cached-test-token" {
				t.Fatalf("bad credential with usable cache = %q, %v; want the cache", got, err)
			}
			fresh := testPool(t, "verify-full")
			if err := configurePool(fresh, []string{"db.example"}, f.mint); err != nil {
				t.Fatal(err)
			}
			_, err := connectPassword(fresh)
			if err == nil ||
				!strings.Contains(err.Error(), "empty or expired database credential") {
				t.Fatalf("bad credential without a cache = %v; want refusal", err)
			}
		})
	}
}

func TestCredentialFallbackWarningIsRateLimited(t *testing.T) {
	shrinkTimings(t, time.Second)
	var warnings []string
	var mu sync.Mutex
	credentialWarn = func(msg string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		warnings = append(warnings, fmt.Sprint(append([]any{msg}, args...)...))
	}
	f := &fakeMint{}
	cfg := seededPool(t, f, "projects/p/branches/b/endpoints/warn", 90*time.Second)
	f.set(fail(errors.New("credential service unavailable")))
	for range 5 {
		if _, err := connectPassword(cfg); err != nil {
			t.Fatal(err)
		}
	}
	if len(warnings) != 1 {
		t.Fatalf("warnings = %d (%q); want one per interval", len(warnings), warnings)
	}
	if strings.Contains(warnings[0], "cached-test-token") {
		t.Fatal("fallback warning logged the database credential")
	}
	if !strings.Contains(warnings[0], "projects/p/branches/b/endpoints/warn") {
		t.Fatalf("warning %q does not name the endpoint", warnings[0])
	}
	state, _ := CredentialStatus("projects/p/branches/b/endpoints/warn")
	if state.ConsecutiveFailures != 5 {
		t.Fatalf("ConsecutiveFailures = %d; want every failed mint counted",
			state.ConsecutiveFailures)
	}
}

func TestCredentialStatusUnknownEndpoint(t *testing.T) {
	state, ok := CredentialStatus("projects/p/branches/b/endpoints/never-configured")
	if ok || state != (CredentialState{}) {
		t.Fatalf("unknown endpoint status = %+v, %v; want none", state, ok)
	}
	f := &fakeMint{}
	seededPool(t, f, "", time.Hour)
	if _, ok := CredentialStatus(""); ok {
		t.Fatal("untracked pool published a status")
	}
}

func TestCredentialWaiterHonorsItsOwnContext(t *testing.T) {
	shrinkTimings(t, 5*time.Second)
	release := make(chan struct{})
	f := &fakeMint{}
	f.set(func(context.Context, int) (*postgres.DatabaseCredential, error) {
		<-release
		return credentialFor("late-test-token", time.Hour), nil
	})
	cfg := testPool(t, "verify-full")
	if err := configurePool(cfg, []string{"db.example"}, f.mint); err != nil {
		t.Fatal(err)
	}
	leader := make(chan error, 1)
	go func() { _, err := connectPassword(cfg); leader <- err }()
	for f.callCount() == 0 {
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	conn := cfg.ConnConfig.Copy()
	err := cfg.BeforeConnect(ctx, conn)
	if !errors.Is(err, context.DeadlineExceeded) || conn.Password != "" {
		t.Fatalf("waiter past its deadline = %v (password %q); want its own timeout",
			err, conn.Password)
	}
	close(release)
	if err := <-leader; err != nil {
		t.Fatalf("leader connect = %v", err)
	}
	if f.callCount() != 1 {
		t.Fatalf("mint calls = %d; want the waiter to start none", f.callCount())
	}
}

// State transitions are covered above: fresh -> refresh window -> fallback
// (failures counted) -> recovered (failures reset), and fallback -> below
// minUsable -> refused.
