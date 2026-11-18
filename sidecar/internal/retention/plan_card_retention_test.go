package retention

import "testing"

// plan-03b: plan cards age from creation on the actions window, like fact
// cards; a card is only a single-use token, the decision lives with the
// plan.
func TestPurgeRulesAgePlanCards(t *testing.T) {
	for _, rule := range purgeRules(allDays(30)) {
		if rule.table == "plan_card_deliveries" {
			if rule.timeCol != "created_at" || rule.days != 30 || rule.extra != "" {
				t.Fatalf("plan_card_deliveries rule = %+v", rule)
			}
			return
		}
	}
	t.Fatal("no purge rule for plan_card_deliveries")
}
