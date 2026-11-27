package inbox

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/review/modelgate"
)

// noInstances knows no database.
type noInstances struct{}

func (noInstances) GetInstance(string) *fleet.DatabaseInstance { return nil }

// review-05a: the model gate's pending G-M1 promotion is an autonomy
// promotion, high risk and admin only. No concurrent access test: the
// source reads one row per call and holds no state.

func TestModelGateItem_IsAHighRiskAdminPromotionWithTheNumbers(t *testing.T) {
	at := time.Date(2026, 11, 17, 8, 0, 0, 0, time.UTC)
	it := modelGateItem("shop", modelgate.Row{ID: 42, Model: "m1", Rule: modelgate.RuleM1,
		Verdict: modelgate.StatePending, EvaluatedAt: at,
		Reason: "G-M1: 50 of 50 reviews; model 40/50 improved"})
	if it.Kind != KindAutonomyPromotion || it.ID != "review-gate-42" ||
		it.Database != "shop" || it.Risk != RiskHigh || !it.CreatedAt.Equal(at) ||
		it.Source != "sage.review_gate" {
		t.Fatalf("item = %+v", it)
	}
	if !strings.Contains(it.Title, "m1") || !strings.Contains(it.Why, "40/50 improved") ||
		it.Href != "#/trust?database=shop&review_gate=42" {
		t.Fatalf("title %q why %q href %q", it.Title, it.Why, it.Href)
	}
	if CanDecide(it.Kind, "operator") || !CanDecide(it.Kind, "admin") {
		t.Fatal("a model gate promotion is not admin-only")
	}
}

func TestAllOf_ConcatenatesAndFailsOnAnyError(t *testing.T) {
	one := SourceFunc(func(context.Context, string) ([]Item, error) {
		return []Item{{ID: "a"}}, nil
	})
	two := SourceFunc(func(context.Context, string) ([]Item, error) {
		return []Item{{ID: "b"}, {ID: "c"}}, nil
	})
	got, err := allOf(one, two).List(context.Background(), "db")
	if err != nil || len(got) != 3 || got[0].ID != "a" || got[2].ID != "c" {
		t.Fatalf("allOf = %+v %v, want a, b, c", got, err)
	}
	boom := errors.New("relation sage.review_gate does not exist")
	bad := SourceFunc(func(context.Context, string) ([]Item, error) { return nil, boom })
	if got, err := allOf(one, bad).List(context.Background(), "db"); !errors.Is(err, boom) ||
		got != nil {
		t.Fatalf("allOf with a failing source = %+v %v, want the error and no items", got,
			err)
	}
	if got, err := allOf().List(context.Background(), "db"); err != nil || len(got) != 0 {
		t.Fatalf("allOf() = %+v %v", got, err)
	}
}

// An offline database lists none: the ledger's promotions still list for
// it, and every other kind reports it as not connected.
func TestModelGatePromotions_NoFleetAndOfflineListNone(t *testing.T) {
	for name, in := range map[string]Instances{"no fleet": nil, "offline": noInstances{}} {
		got, err := ModelGatePromotions(in).List(context.Background(), "shop")
		if err != nil || len(got) != 0 {
			t.Fatalf("%s: %+v %v, want none", name, got, err)
		}
	}
}
