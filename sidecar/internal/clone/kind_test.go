package clone

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// lab-05a: every clone says what kind it is and what it costs before it
// is made; a full copy (hours, a whole instance) is made only on an
// explicit opt-in, and a provider that cannot say is treated as one.

// kindInner is a provider with a declared capability that counts creates.
type kindInner struct {
	capability  Capability
	describeErr error
	creates     atomic.Int32
}

func (k *kindInner) Create(context.Context, CloneSpec) (Clone, error) {
	k.creates.Add(1)
	return Clone{ID: "c", DSN: "postgres://clone.invalid/app", CreatedFrom: time.Now()}, nil
}
func (k *kindInner) Destroy(context.Context, Clone) error { return nil }
func (k *kindInner) SnapshotAge(context.Context) (time.Duration, error) {
	return 0, nil
}
func (k *kindInner) Describe(context.Context) (Capability, error) {
	return k.capability, k.describeErr
}

// opaqueInner implements only Provider: it says nothing of its kind.
type opaqueInner struct{ creates atomic.Int32 }

func (o *opaqueInner) Create(context.Context, CloneSpec) (Clone, error) {
	o.creates.Add(1)
	return Clone{ID: "c", DSN: "postgres://clone.invalid/app"}, nil
}
func (o *opaqueInner) Destroy(context.Context, Clone) error { return nil }
func (o *opaqueInner) SnapshotAge(context.Context) (time.Duration, error) {
	return 0, nil
}

func TestKind_ProviderWithoutDescriberIsFullCopy(t *testing.T) {
	got, err := DescribeOf(context.Background(), &opaqueInner{})
	if err != nil || got.Kind != KindFullCopy {
		t.Fatalf("DescribeOf(opaque) = %+v, %v; want full_copy (fail closed)", got, err)
	}
	if _, err := DescribeOf(context.Background(), nil); err == nil {
		t.Fatal("DescribeOf(nil) succeeded")
	}
}

func TestKind_ExistingAdaptersDescribeThemselves(t *testing.T) {
	ctx := context.Background()
	lake := newLakebaseForTest(t, testLakebaseConfig(&fakeBranchAPI{}))
	snap, err := NewSnapshotProvider(&fakeSnapshotAPI{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		p    Provider
		want Kind
	}{
		"dle": {&DLEProvider{}, KindThinClone}, "lakebase": {lake, KindBranch},
		"snapshot": {snap, KindFullCopy},
	} {
		if _, ok := tc.p.(Describer); !ok {
			t.Errorf("%s does not implement Describer", name)
			continue
		}
		got, err := DescribeOf(ctx, tc.p)
		if err != nil || got.Kind != tc.want || got.TypicalReady <= 0 {
			t.Errorf("%s describes as %+v, %v; want %s with a typical ready time", name,
				got, err, tc.want)
		}
	}
	if got, _ := DescribeOf(ctx, lake); got.MaxTTL != time.Hour {
		t.Errorf("lakebase MaxTTL = %s, want its configured branch TTL 1h", got.MaxTTL)
	}
}

func TestKind_EstimateUnpricedWithoutRates(t *testing.T) {
	c := Capability{Kind: KindBranch}.Estimate(90*time.Minute, nil)
	if c.Priced || c.Amount != 0 || c.InstanceHours != 1.5 || c.Kind != KindBranch {
		t.Fatalf("estimate without rates = %+v, want unpriced 1.5 instance-hours", c)
	}
	if !strings.Contains(c.Label, "unpriced") || !strings.Contains(c.Label, "1.50") {
		t.Fatalf("label %q does not say unpriced with the instance-hours", c.Label)
	}
}

func TestKind_EstimatePricedNamesItsSource(t *testing.T) {
	rate := &Rate{PerHour: 2.4, Currency: "USD", AsOf: "2026-11-01", Source: "orders"}
	c := Capability{Kind: KindFullCopy}.Estimate(2*time.Hour, rate)
	if !c.Priced || c.Amount != 4.8 || c.Currency != "USD" || c.Source != "orders" {
		t.Fatalf("priced estimate = %+v, want 4.80 USD from orders", c)
	}
	for _, want := range []string{"4.80 USD", "orders", "2026-11-01", "2.00 instance-hours"} {
		if !strings.Contains(c.Label, want) {
			t.Errorf("label %q lacks %q", c.Label, want)
		}
	}
}

func TestKind_EstimateBoundaries(t *testing.T) {
	thin := Capability{Kind: KindThinClone}.Estimate(time.Hour, &Rate{PerHour: 3,
		Currency: "USD", AsOf: "2026-11-01", Source: "orders"})
	if thin.InstanceHours != 0 || thin.Amount != 0 || !strings.Contains(thin.Label, "thin") {
		t.Fatalf("a thin clone adds no instance: %+v", thin)
	}
	zero := Capability{Kind: KindBranch}.Estimate(0, nil)
	if zero.InstanceHours != 0 || zero.Label == "" {
		t.Fatalf("zero hold = %+v", zero)
	}
	neg := Capability{Kind: KindBranch}.Estimate(-time.Hour, nil)
	if neg.InstanceHours != 0 {
		t.Fatalf("a negative hold estimated %v instance-hours", neg.InstanceHours)
	}
	unpricedRate := Capability{Kind: KindBranch}.Estimate(time.Hour, &Rate{})
	if unpricedRate.Priced {
		t.Fatalf("a rate without a currency priced the clone: %+v", unpricedRate)
	}
}

func TestKind_CheckOptIn(t *testing.T) {
	ctx := context.Background()
	full := &kindInner{capability: Capability{Kind: KindFullCopy}}
	if _, err := CheckOptIn(ctx, full, false); !errors.Is(err, ErrFullCopyNotAllowed) ||
		!strings.Contains(err.Error(), "clone.allow_full_copy") {
		t.Fatalf("full copy without opt-in: %v", err)
	}
	if got, err := CheckOptIn(ctx, full, true); err != nil || got.Kind != KindFullCopy {
		t.Fatalf("full copy with opt-in: %+v, %v", got, err)
	}
	if _, err := CheckOptIn(ctx, &opaqueInner{}, false); !errors.Is(err,
		ErrFullCopyNotAllowed) {
		t.Fatalf("an undescribed provider was not treated as full copy: %v", err)
	}
	schema := &kindInner{capability: Capability{Kind: KindSchemaOnly}}
	if _, err := CheckOptIn(ctx, schema, true); !errors.Is(err, ErrCloneKindRefused) {
		t.Fatalf("schema_only was accepted: %v", err)
	}
	unknown := &kindInner{capability: Capability{Kind: "magic"}}
	if _, err := CheckOptIn(ctx, unknown, true); !errors.Is(err, ErrCloneKindRefused) {
		t.Fatalf("an unknown kind was accepted: %v", err)
	}
	boom := errors.New("control plane down")
	broken := &kindInner{describeErr: boom}
	if _, err := CheckOptIn(ctx, broken, true); !errors.Is(err, boom) {
		t.Fatalf("a describe error was not propagated: %v", err)
	}
	for _, k := range []Kind{KindThinClone, KindBranch, KindCopyOnWrite} {
		inner := &kindInner{capability: Capability{Kind: k}}
		if _, err := CheckOptIn(ctx, inner, false); err != nil {
			t.Errorf("%s needs no opt-in: %v", k, err)
		}
	}
}

func TestKind_ReceiptingRefusesFullCopyBeforeAnyReceipt(t *testing.T) {
	inner := &opaqueInner{}
	// A nil store would refuse with ErrNoReceiptStore; the opt-in is
	// checked first, so nothing at all is asked of the store or provider.
	p, err := NewReceiptingProvider(inner, nil, rehearsalConfig())
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Create(context.Background(), CloneSpec{})
	if !errors.Is(err, ErrFullCopyNotAllowed) || inner.creates.Load() != 0 {
		t.Fatalf("create = %v after %d provider calls; want refused with none", err,
			inner.creates.Load())
	}
}

func TestKind_DLEStatusErrorsClassifyThrottling(t *testing.T) {
	var status atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,
		_ *http.Request) {
		w.WriteHeader(int(status.Load()))
	}))
	defer server.Close()
	p := newDLEProviderForTest(t, server)
	for code, want := range map[int32]bool{429: true, 500: true, 503: true, 400: false,
		404: false} {
		status.Store(code)
		_, err := p.Create(context.Background(), CloneSpec{})
		if err == nil || Throttled(err) != want {
			t.Errorf("status %d: err %v, throttled %v, want %v", code, err, Throttled(err),
				want)
		}
	}
	if Throttled(nil) || Throttled(errors.New("429 in text only")) {
		t.Fatal("an error without a provider status was classified as throttling")
	}
}

func TestKind_CreateBackoffGrowsAndResets(t *testing.T) {
	now := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	b := NewCreateBackoff(func() time.Time { return now })
	if err := b.Allow(); err != nil {
		t.Fatalf("a fresh backoff refused: %v", err)
	}
	b.Observe(&ProviderStatusError{Op: "test create", StatusCode: 429})
	if err := b.Allow(); !errors.Is(err, ErrProviderBackoff) {
		t.Fatalf("after a 429: %v", err)
	}
	now = now.Add(time.Minute - time.Second)
	if err := b.Allow(); !errors.Is(err, ErrProviderBackoff) {
		t.Fatal("allowed before the first backoff (1m) ended")
	}
	now = now.Add(time.Second)
	if err := b.Allow(); err != nil {
		t.Fatalf("refused at the end of the first backoff: %v", err)
	}
	b.Observe(&ProviderStatusError{Op: "test create", StatusCode: 502})
	now = now.Add(time.Minute)
	if err := b.Allow(); !errors.Is(err, ErrProviderBackoff) {
		t.Fatal("the second backoff did not double to 2m")
	}
	now = now.Add(time.Minute)
	b.Observe(errors.New("bad request")) // not a throttle: leaves the backoff
	if err := b.Allow(); err != nil {
		t.Fatalf("refused after the doubled backoff: %v", err)
	}
	b.Observe(nil)
	b.Observe(&ProviderStatusError{Op: "test create", StatusCode: 429})
	now = now.Add(time.Minute)
	if err := b.Allow(); err != nil {
		t.Fatalf("a success did not reset the backoff to 1m: %v", err)
	}
	for range 20 {
		b.Observe(&ProviderStatusError{Op: "test create", StatusCode: 429})
	}
	now = now.Add(maxCreateBackoff)
	if err := b.Allow(); err != nil {
		t.Fatalf("the backoff exceeded its %s cap: %v", maxCreateBackoff, err)
	}
	if SharedCreateBackoff("dle", "a") != SharedCreateBackoff("dle", "a") ||
		SharedCreateBackoff("dle", "a") == SharedCreateBackoff("dle", "b") {
		t.Fatal("the shared backoff is not one per adapter and scope")
	}
}
