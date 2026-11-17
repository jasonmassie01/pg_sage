package config

import (
	"strings"
	"testing"
)

// lab-03b: sre.autonomy.canary.stage_wait_hours pauses a fleet rollout
// between members; 24 by default with no config at all, 0 (no pause) to
// 720, anything else refused naming the key.
func TestCanaryStageWaitHoursDefault(t *testing.T) {
	if got := defaultSREAutonomyConfig().Canary.StageWaitHours; got != 24 {
		t.Fatalf("default stage_wait_hours = %d, want 24", got)
	}
}

func TestCanaryStageWaitHoursRange(t *testing.T) {
	for _, c := range []struct {
		hours int
		ok    bool
	}{{-1, false}, {0, true}, {1, true}, {168, true}, {720, true}, {721, false}} {
		a := defaultSREAutonomyConfig()
		a.Canary.StageWaitHours = c.hours
		err := a.validate()
		if (err == nil) != c.ok {
			t.Fatalf("stage_wait_hours %d: err %v, want ok=%v", c.hours, err, c.ok)
		}
		if err != nil && !strings.Contains(err.Error(), "stage_wait_hours") {
			t.Fatalf("stage_wait_hours %d: error %q does not name the key", c.hours, err)
		}
	}
}
