package executor

import (
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/pgconf"
	"github.com/pg-sage/sidecar/internal/verify"
)

// advisors-05: a WAL change is judged by the requested share of
// checkpoints, so it no longer ends unverifiable.

// No concurrent access tests: the verdict functions are pure.

func TestOutcomeMetricFor_WALSettings(t *testing.T) {
	for sql, want := range map[string]string{
		"ALTER SYSTEM SET max_wal_size = '4GB'":               metricRequestedCheckpoints,
		"ALTER SYSTEM SET checkpoint_timeout = '15min'":       metricRequestedCheckpoints,
		"ALTER SYSTEM SET min_wal_size = '1GB'":               "",
		"ALTER SYSTEM SET checkpoint_completion_target = 0.9": "",
	} {
		stmt, ok := pgconf.ParseAlterSystem(sql)
		if !ok {
			t.Fatalf("parse %q", sql)
		}
		metric, table := outcomeMetricFor(&configChange{kind: configKindGUC, guc: stmt})
		if metric != want || table != "" {
			t.Errorf("%s: metric %q table %q, want %q", sql, metric, table, want)
		}
	}
	if p := configPrediction(metricRequestedCheckpoints); p.Metric != metricRequestedCheckpoints ||
		p.ExpectedChangePct == nil {
		t.Errorf("prediction = %+v, want a rule-based requested_checkpoints bar", p)
	}
}

func ckpt(req, timed, epoch float64) map[string]float64 {
	return map[string]float64{"requested": req, "timed": timed, "epoch": epoch}
}

func TestJudgeRequestedCheckpoints(t *testing.T) {
	// Terminal marks only an insufficient verdict more time cannot change
	// (metricJudgement); a decided verdict leaves it false.
	cases := []struct {
		name          string
		before, after map[string]float64
		verdict       string
		terminal      bool
		reason        string
	}{
		// Lifetime share 0.9; the window has 0 of 3: improved.
		{"pressure gone", ckpt(90, 10, 1), ckpt(90, 13, 1), verify.OutcomeImproved, false,
			"fell"},
		// Window share 0.45 is exactly half of 0.9: improved.
		{"halved boundary", ckpt(90, 10, 1), ckpt(99, 21, 1), verify.OutcomeImproved, false,
			"fell"},
		// 10 of 20 is 0.5 > 0.45 and not under 0.20: neutral.
		{"not enough", ckpt(90, 10, 1), ckpt(100, 20, 1), verify.OutcomeNeutral, false,
			"did not fall"},
		{"too few checkpoints", ckpt(90, 10, 1), ckpt(91, 11, 1),
			verify.OutcomeInsufficient, false, "too few"},
		{"epoch moved", ckpt(90, 10, 1), ckpt(95, 15, 2), verify.OutcomeInsufficient, true,
			"reset"},
		{"counter fell", ckpt(90, 10, 1), ckpt(1, 30, 1), verify.OutcomeInsufficient, true,
			"reset"},
		{"no pressure before", ckpt(1, 99, 1), ckpt(1, 120, 1),
			verify.OutcomeInsufficient, true, "nothing to improve"},
		{"no checkpoints before", ckpt(0, 0, 1), ckpt(5, 5, 1),
			verify.OutcomeInsufficient, true, "no checkpoints"},
		{"counters missing", map[string]float64{}, ckpt(1, 1, 1),
			verify.OutcomeUnverifiable, true, "missing"},
	}
	for _, c := range cases {
		j := judgeRequestedCheckpoints(c.before, c.after)
		if j.Verdict != c.verdict || j.Terminal != c.terminal ||
			!strings.Contains(j.Reason, c.reason) {
			t.Errorf("%s: %s terminal=%v %q, want %s terminal=%v containing %q", c.name,
				j.Verdict, j.Terminal, j.Reason, c.verdict, c.terminal, c.reason)
		}
		if j.Verdict == verify.OutcomeRegressed {
			t.Errorf("%s: a WAL change is never judged a regression", c.name)
		}
	}
	j := judgeOutcome(outcomeBaseline{Metric: metricRequestedCheckpoints,
		Counters: ckpt(90, 10, 1)}, ckpt(90, 13, 1), time.Time{})
	if j.Metric != metricRequestedCheckpoints || j.Verdict != verify.OutcomeImproved ||
		j.Before != 0.9 || j.After != 0 {
		t.Errorf("judgeOutcome = %+v, want improved 0.9 -> 0", j)
	}
}
