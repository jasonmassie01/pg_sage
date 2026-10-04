package earned

import (
	"fmt"
	"strings"
	"time"
)

// Promotion coach (2026-10-02 roadmap Phase 1.1): every unmet check
// carries one plain instruction with the counts behind it and, where the
// rule implies one, the time it will be met by itself. The instruction
// explains the bar; it never lowers it.

// agentReviews says which reviews count (coordinator decision 2026-10-02).
const agentReviews = "reviews an agent records through MCP do not count."

// maxAcceptedSearch bounds the search for the accepted reviews a shadow
// record still needs.
const maxAcceptedSearch = 10000

// guide fills c.How (and c.ETA) for an unmet check.
func guide(th Thresholds, ev Evidence, c *Check) {
	if c.How != "" {
		return // the check wrote its own instruction (self-initiated classes)
	}
	switch {
	case strings.HasPrefix(c.Name, "shadow_"):
		guideShadow(th, ev, c)
	case strings.HasPrefix(c.Name, "bench_"):
		guideBench(th, ev, c)
	default:
		guideOther(th, ev, c)
	}
}

func guideShadow(th Thresholds, ev Evidence, c *Check) {
	sh, window := ev.Shadow, humanDuration(th.ShadowDuration)
	switch c.Name {
	case "shadow_duration":
		if sh.FirstReviewAt.IsZero() {
			c.How = fmt.Sprintf("A person accepts or rejects a concluded or inconclusive "+
				"%s investigation in Cases (or the REST API) to start the %s shadow "+
				"window; %s", ev.Family, window, agentReviews)
			return
		}
		eta := sh.FirstReviewAt.Add(th.ShadowDuration)
		c.How = fmt.Sprintf("Keep reviewing: the first review was %s ago and the %s "+
			"shadow window has %s to go.", humanDuration(ev.At.Sub(sh.FirstReviewAt)),
			window, humanDuration(eta.Sub(ev.At)))
		c.ETA = &eta
	case "shadow_volume":
		c.How = fmt.Sprintf("Have a person review %d more concluded or inconclusive %s "+
			"investigations in Cases (%d of %d in the last %s); %s",
			th.ShadowMinReviewed-sh.Reviewed, ev.Family, sh.Reviewed, th.ShadowMinReviewed,
			window, agentReviews)
	case "shadow_acceptance":
		need := acceptedNeeded(sh.Accepted, sh.Reviewed, th.ShadowMinAccepted)
		bar := fmt.Sprintf("%d of %d reviews in the last %s were accepted (needs %.0f%%)",
			sh.Accepted, sh.Reviewed, window, th.ShadowMinAccepted*100)
		if need < 0 {
			c.How = bar + "; the rejected reviews must leave the window first."
			return
		}
		c.How = fmt.Sprintf("%s: %d more accepted reviews by a person, or wait for "+
			"rejected ones to leave the window.", bar, need)
	}
}

func guideBench(th Thresholds, ev Evidence, c *Check) {
	switch {
	case ev.Bench == nil || len(gatedCells(ev)) == 0:
		c.How = fmt.Sprintf("Run the bench locally on a clone (Run bench locally, admin), "+
			"upgrade to a release that ships its signed report, upload a PGIncidentBench "+
			"report on this page (admin) or point sre.autonomy.bench_results_path at the CI "+
			"report; it must score %s on a gated arm for this pg_sage build.", ev.Family)
	case c.Name == "bench_fresh":
		c.How = fmt.Sprintf("The newest bench report is %s old; run the bench locally on "+
			"a clone or ingest one newer than %s.",
			humanDuration(ev.At.Sub(ev.Bench.GeneratedAt)), humanDuration(th.BenchMaxAge))
	default:
		c.How = fmt.Sprintf("The bench report scores %s on %s (needs %s); run "+
			"PGIncidentBench again after improving the arm, then ingest the new report.",
			c.Observed, ev.Family, c.Required)
	}
}

func guideOther(th Thresholds, ev Evidence, c *Check) {
	switch c.Name {
	case "no_safety_violations":
		c.How = fmt.Sprintf("%d harmful or unsafe %s outcomes on this database in the "+
			"safety window; promotion resumes when they age out.", ev.FamilyViolations,
			ev.Family)
		c.ETA = ev.ViolationsClearAt
	case "live_l2_recoveries":
		c.How = fmt.Sprintf("Approve %d more L2 handoffs of %s/%s and let their "+
			"verification finish (%d verified so far; %d unverified outcomes earn "+
			"nothing).", th.MinL2Recoveries-ev.Live.VerifiedL2, ev.Family, ev.Class,
			ev.Live.VerifiedL2, ev.Live.Unverified)
	case "no_harmful_actions":
		c.How = "This pair had a harmful outcome on this database; by design it never " +
			"reaches L3."
	case "game_day_safe_pass":
		c.How = fmt.Sprintf("Game days on this database scored %s Safe Pass (needs %s); "+
			"fix the failing scenarios and run another game day.", c.Observed, c.Required)
	case "family_ships", "class_applicable":
		c.How = "Only a shipped family with a class that remediates it earns autonomy; " +
			"this pair stays at L1."
	default:
		c.How = fmt.Sprintf("%s: observed %s, needs %s.", c.Name, c.Observed, c.Required)
	}
}

// acceptedNeeded is how many more accepted reviews lift accepted of
// reviewed to rate, by the same test the check applies; -1 when no number
// of accepted reviews can (a 100% bar with a rejection in the window).
func acceptedNeeded(accepted, reviewed int, rate float64) int {
	for more := 0; more <= maxAcceptedSearch; more++ {
		r, ok := Metric{K: accepted + more, N: reviewed + more}.Rate()
		if ok && r >= rate {
			return more
		}
	}
	return -1
}

// humanDuration renders d for an instruction: whole days, hours from two
// hours up, minutes below.
func humanDuration(d time.Duration) string {
	const day = 24 * time.Hour
	switch {
	case d <= 0:
		return "0 min"
	case d == day:
		return "1 day"
	case d%day == 0 || d >= 2*day:
		return fmt.Sprintf("%d days", int(d.Round(day)/day))
	case d%time.Hour == 0 || d >= 2*time.Hour:
		return fmt.Sprintf("%d h", int(d.Round(time.Hour)/time.Hour))
	}
	return fmt.Sprintf("%d min", int(d.Round(time.Minute)/time.Minute))
}
