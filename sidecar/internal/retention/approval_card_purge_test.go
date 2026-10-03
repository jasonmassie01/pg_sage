package retention

import "testing"

// Approval-card deliveries age from their follow-up: a card still waiting
// for its verdict (followed_up_at NULL) never matches the age predicate.
func TestPurgeRulesAgeApprovalCardsFromTheirFollowUp(t *testing.T) {
	for _, rule := range purgeRules(allDays(30)) {
		if rule.table != "approval_card_deliveries" {
			continue
		}
		if rule.timeCol != "followed_up_at" || rule.days != 30 || rule.extra != "" {
			t.Fatalf("approval_card_deliveries rule = %+v, want followed_up_at and the "+
				"actions window", rule)
		}
		return
	}
	t.Fatal("sage.approval_card_deliveries has no purge rule")
}
