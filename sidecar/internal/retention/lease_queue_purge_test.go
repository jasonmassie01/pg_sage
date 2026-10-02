package retention

import (
	"strings"
	"testing"
)

// The serialize_mode=queue table ages out like the change leases it
// feeds, but an entry still waiting for its turn is never purged.
func TestPurgeRulesKeepWaitingLeaseQueueEntries(t *testing.T) {
	for _, rule := range purgeRules(allDays(30)) {
		if rule.table != "lease_queue" {
			continue
		}
		if rule.timeCol != "enqueued_at" || rule.days != 30 ||
			!strings.Contains(rule.extra, "state <> 'waiting'") {
			t.Fatalf("lease_queue rule = %+v, want enqueued_at, the actions window and "+
				"waiting entries kept", rule)
		}
		return
	}
	t.Fatal("sage.lease_queue has no purge rule")
}
