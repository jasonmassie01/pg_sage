package chatops

import (
	"errors"
	"testing"
)

// plan-03b: a plan card's buttons carry only the plan decision and an
// opaque token naming one plan card sent to one channel. No concurrent
// access test here: parsing is pure (plan_card_db_test.go races Consume).

func TestParseSlackPlanDecisions(t *testing.T) {
	for actionID, want := range map[string]PlanChoice{
		SlackPlanApproveAllAction: PlanApproveAll, SlackPlanApproveUpToAction: PlanApproveUpTo,
		SlackPlanApproveNextAction: PlanApproveNext, SlackPlanRejectAction: PlanReject,
	} {
		got, err := ParseSlack(slackForm(t, slackPayload(actionID, cardToken)))
		if err != nil {
			t.Fatalf("%s: %v", actionID, err)
		}
		wantDecision := DecisionApprove
		if want == PlanReject {
			wantDecision = DecisionDeny
		}
		if got.PlanChoice != want || got.PlanToken != cardToken || got.CardToken != "" ||
			got.FactToken != "" || got.Outdated || got.Decision != wantDecision ||
			got.UserID != "U0042" {
			t.Fatalf("%s parsed as %+v", actionID, got)
		}
	}
	for _, bad := range []string{"short", "", cardToken + "x", "AAAAAAAAAAAAAAAAAAAA!!"} {
		if _, err := ParseSlack(slackForm(t, slackPayload(SlackPlanApproveAllAction,
			bad))); !errors.Is(err, ErrMalformed) {
			t.Fatalf("bad plan token %q: %v", bad, err)
		}
	}
}

func TestParseTelegramPlanDecisions(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range []PlanChoice{PlanApproveAll, PlanApproveUpTo, PlanApproveNext,
		PlanReject} {
		data := PlanCallbackData(c, cardToken)
		if len(data) > 64 || seen[data] {
			t.Fatalf("callback data %q: %d bytes or not distinct", data, len(data))
		}
		seen[data] = true
		got, err := ParseTelegram(telegramUpdate(data))
		if err != nil {
			t.Fatalf("%s: %v", c, err)
		}
		if got.PlanChoice != c || got.PlanToken != cardToken || got.CardToken != "" ||
			got.FactToken != "" || got.MessageID != 77 {
			t.Fatalf("%s parsed as %+v", c, got)
		}
	}
	if PlanCallbackData(PlanApproveAll, cardToken) == CardCallbackData(DecisionApprove,
		cardToken) || PlanCallbackData(PlanReject, cardToken) == FactCallbackData(
		DecisionDeny, cardToken) {
		t.Fatal("plan callback data must differ from card and fact data")
	}
	for _, data := range []string{"sage:px:" + cardToken, "sage:pa:short", "sage:p",
		"sage:pa" + cardToken, "sage:pa:" + cardToken + "x", "sage:pa:"} {
		if _, err := ParseTelegram(telegramUpdate(data)); !errors.Is(err, ErrMalformed) {
			t.Fatalf("%q: %v", data, err)
		}
	}
}

func TestPlanChoice_Valid(t *testing.T) {
	for _, c := range []PlanChoice{PlanApproveAll, PlanApproveUpTo, PlanApproveNext,
		PlanReject} {
		if !c.Valid() {
			t.Errorf("%s is not valid", c)
		}
	}
	for _, c := range []PlanChoice{"", "approve", "APPROVE_ALL", "approve_most"} {
		if c.Valid() {
			t.Errorf("%q is valid", c)
		}
	}
}

func TestPlanCardDelivery_Offers(t *testing.T) {
	d := PlanCardDelivery{Offered: []PlanChoice{PlanApproveNext, PlanReject}}
	if !d.Offers(PlanReject) || !d.Offers(PlanApproveNext) || d.Offers(PlanApproveAll) ||
		d.Offers("") {
		t.Fatalf("offers wrong for %v", d.Offered)
	}
	var empty PlanCardDelivery
	if empty.Offers(PlanReject) {
		t.Fatal("an empty delivery offers reject")
	}
}
