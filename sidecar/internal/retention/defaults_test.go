package retention

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// TestRetentionDefaults replaces TestRetentionConfig_DefaultValues, which
// built a config from the literals 30/90/180/14 and asserted those same
// literals back, so it passed while the real defaults were 90/180/365/90
// (ci-11, the default-value masking pattern). It reads the defaults a
// sidecar starts with when retention is absent from the config file.
func TestRetentionDefaults(t *testing.T) {
	got := config.DefaultConfig().Retention
	for _, tc := range []struct {
		field     string
		got, want int
		doc       int // configuration.md
	}{
		{"snapshots_days", got.SnapshotsDays, config.DefaultRetentionSnapshotsDays, 90},
		{"findings_days", got.FindingsDays, config.DefaultRetentionFindingsDays, 180},
		{"actions_days", got.ActionsDays, config.DefaultRetentionActionsDays, 365},
		{"explains_days", got.ExplainsDays, config.DefaultRetentionExplainsDays, 90},
	} {
		if tc.got != tc.want {
			t.Errorf("DefaultConfig().Retention %s = %d, want DefaultRetention constant %d",
				tc.field, tc.got, tc.want)
		}
		if tc.want != tc.doc {
			t.Errorf("retention.%s default = %d, configuration.md documents %d",
				tc.field, tc.want, tc.doc)
		}
		// purgeTable treats a non-positive day count as "never purge".
		if tc.got <= 0 {
			t.Errorf("retention.%s default = %d: a zero default disables the purge",
				tc.field, tc.got)
		}
	}
}
