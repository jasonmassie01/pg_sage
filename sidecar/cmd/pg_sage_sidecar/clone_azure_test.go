package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/azure"
	"github.com/pg-sage/sidecar/internal/clone"
	"github.com/pg-sage/sidecar/internal/config"
)

// lab-05g: clone.provider azure_restore is wired into the migration
// factory and the reaper; a rehearsal on it is bounded by
// clone.restore_deadline plus the rehearsal itself. No test reaches Azure
// or reads this machine's credentials.

const azureCloneSub = "0f1e2d3c-4b5a-4697-8877-665544332211"

func azureRestoreCfg() config.CloneProviderConfig {
	return config.CloneProviderConfig{Provider: "azure_restore", MaxCloneAgeMinutes: 60,
		AllowFullCopy: true, RestoreDeadlineMinutes: 120,
		Azure: config.CloneAzureConfig{SubscriptionID: azureCloneSub,
			ResourceGroup: "rg-shop-prod", Server: "shop"}}
}

// stubAzureCredential replaces the ARM credential chain for one test and
// counts how often it is built.
func stubAzureCredential(t *testing.T, tok string, err error) *int {
	t.Helper()
	loads := 0
	old := newAzureTokenSource
	newAzureTokenSource = func() (azure.TokenSource, error) {
		loads++
		if err != nil {
			return nil, err
		}
		return func(context.Context) (string, error) { return tok, nil }, nil
	}
	t.Cleanup(func() { newAzureTokenSource = old })
	return &loads
}

func TestConfiguredCloneProviderBuildsAzureRestore(t *testing.T) {
	loads := stubAzureCredential(t, "tok", nil)
	p, err := configuredMCPCloneProvider(azureRestoreCfg())
	if err != nil {
		t.Fatal(err)
	}
	sp, ok := p.(*clone.SnapshotProvider)
	if !ok {
		t.Fatalf("azure_restore built %T", p)
	}
	c, err := sp.Describe(context.Background())
	if err != nil || c.Kind != clone.KindFullCopy || c.MaxTTL != 3*time.Hour+15*time.Minute {
		t.Fatalf("azure_restore capability = %+v, %v", c, err)
	}
	if _, ok := p.(clone.Finder); !ok {
		t.Fatal("azure_restore cannot find an uncertain clone by name")
	}
	if *loads != 0 {
		t.Fatalf("building the provider loaded the Azure credential %d times", *loads)
	}
	bad := azureRestoreCfg()
	bad.Azure.Server = "Shop"
	if p, err := configuredMCPCloneProvider(bad); err == nil || p != nil {
		t.Fatalf("an invalid azure block built %v, %v", p, err)
	}
}

func TestCloneReceiptScopeOfAzureRestoreIsTheSubscriptionAndGroup(t *testing.T) {
	want := "azure/" + azureCloneSub + "/rg-shop-prod"
	if got := cloneReceiptScope(azureRestoreCfg()); got != want {
		t.Fatalf("azure_restore receipt scope = %q, want %s", got, want)
	}
	c := azureRestoreCfg()
	c.Azure.SubscriptionID = strings.ToUpper(azureCloneSub)
	if got := cloneReceiptScope(c); got != want {
		t.Fatalf("uppercase subscription scope = %q, want %s", got, want)
	}
}

// Boundary: the rehearsal bound covers the restore deadline and then the
// rehearsal; the receipt outlives both by the usual margin.
func TestAzureRestoreRehearsalBoundAndReceiptTTL(t *testing.T) {
	for _, tc := range []struct{ deadline, bound int }{{30, 90}, {120, 180}, {720, 780}} {
		c := azureRestoreCfg()
		c.RestoreDeadlineMinutes = tc.deadline
		want := time.Duration(tc.bound) * time.Minute
		if got := rehearsalMaxDuration(c); got != want {
			t.Errorf("deadline %d: bound %s, want %s", tc.deadline, got, want)
		}
		if got := rehearsalReceiptTTL(c); got != want+15*time.Minute {
			t.Errorf("deadline %d: receipt TTL %s, want %s", tc.deadline, got,
				want+15*time.Minute)
		}
	}
}

// The reaper's adapter has no production pool: a create is refused before
// the Azure credential chain is loaded.
func TestAzureRestoreWithoutProductionNeverCreates(t *testing.T) {
	loads := stubAzureCredential(t, "tok", nil)
	p, err := azureRestoreProvider(azureRestoreCfg(), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Create(context.Background(),
		clone.CloneSpec{ResourceName: "sage-rehearsal-0a1b2c3d"})
	if !errors.Is(err, clone.ErrCloneUnavailable) || !strings.Contains(err.Error(),
		"production") || *loads != 0 {
		t.Fatalf("create without a production pool = %v (credential loads %d)", err, *loads)
	}
}

// Nil/empty: a credential chain that cannot be built makes every call
// unavailable; it is built once and reused.
func TestAzureCloneTokenIsLoadedOnceAndFailsClosed(t *testing.T) {
	loads := stubAzureCredential(t, "", errors.New("DefaultAzureCredential: no credential"))
	tok := azureCloneToken()
	for i := 0; i < 2; i++ {
		if got, err := tok(context.Background()); err == nil || got != "" ||
			!strings.Contains(err.Error(), "no credential") {
			t.Fatalf("call %d: token %q, err %v; want the chain's error", i, got, err)
		}
	}
	if *loads != 1 {
		t.Fatalf("credential chain built %d times, want once", *loads)
	}
	loads = stubAzureCredential(t, "arm-token", nil)
	tok = azureCloneToken()
	for i := 0; i < 3; i++ {
		if got, err := tok(context.Background()); err != nil || got != "arm-token" {
			t.Fatalf("call %d: token %q, %v", i, got, err)
		}
	}
	if *loads != 1 {
		t.Fatalf("credential chain built %d times, want once", *loads)
	}
}

// Game days never make a full copy: with azure_restore and no local
// database they stay off.
func TestGameDayProviderNeverPicksAzureRestore(t *testing.T) {
	p, name, err := gameDayProvider(config.SREGameDaysConfig{Enabled: true},
		azureRestoreCfg(), localTargetGuard{})
	if err != nil || p != nil || name == clone.AzureRestoreAdapter {
		t.Fatalf("game day provider = %v, %q, %v; want none", p, name, err)
	}
}

func TestAzureProductionIsNilWithoutAPool(t *testing.T) {
	if azureProduction(nil) != nil {
		t.Fatal("a nil pool has a production identity")
	}
}
