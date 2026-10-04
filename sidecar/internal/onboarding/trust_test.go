package onboarding

import (
	"strings"
	"testing"
)

// No concurrent access tests: TrustGuide is a pure function of its input.

func bp(b bool) *bool           { return &b }
func fp(f float64) *float64     { return &f }
func joined(ss []string) string { return strings.Join(ss, "\n") }

func guideInput(current string) GuideInput {
	return GuideInput{Current: current, SafeRampHours: 192, ModerateRampHours: 744,
		Tier3Safe: true, Tier3Moderate: false,
		Grants: GrantStatus{Role: "sage_agent", Monitor: bp(true), ReadAllStats: bp(true),
			SageSchema: bp(true), SignalBackend: bp(false), TablesOwned: 3, TablesTotal: 10,
			AlterSystem: bp(false)}}
}

func level(levels []Level, name string) Level {
	for _, l := range levels {
		if l.Level == name {
			return l
		}
	}
	return Level{}
}

func grant(l Level, name string) Grant {
	for _, g := range l.Grants {
		if g.Name == name {
			return g
		}
	}
	return Grant{}
}

func TestTrustGuideLevelsInOrder(t *testing.T) {
	levels := TrustGuide(guideInput("observation"))
	if len(levels) != 3 || levels[0].Level != "observation" || levels[1].Level != "advisory" ||
		levels[2].Level != "autonomous" {
		t.Fatalf("levels = %+v", levels)
	}
	if !levels[0].Current || levels[1].Current || levels[2].Current {
		t.Fatalf("current flags = %v %v %v", levels[0].Current, levels[1].Current,
			levels[2].Current)
	}
	for _, l := range levels {
		if l.Title == "" || len(l.Allows) == 0 || len(l.Never) == 0 || len(l.Grants) == 0 {
			t.Fatalf("level %s is incomplete: %+v", l.Level, l)
		}
	}
}

func TestObservationOnlyObserves(t *testing.T) {
	obs := level(TrustGuide(guideInput("observation")), "observation")
	never := strings.ToLower(joined(obs.Never))
	if !strings.Contains(never, "outside the sage schema") {
		t.Fatalf("observation never = %q, want no change outside the sage schema", never)
	}
	for _, g := range obs.Grants {
		if strings.Contains(g.SQL, "pg_signal_backend") || strings.Contains(g.SQL, "OWNER") {
			t.Fatalf("observation asks for a write grant: %+v", g)
		}
	}
	if g := grant(obs, GrantMonitor); g.Present == nil || !*g.Present ||
		!strings.Contains(g.SQL, "GRANT pg_monitor TO sage_agent") {
		t.Fatalf("monitor grant = %+v", g)
	}
}

func TestAdvisoryStatesRampAndGrants(t *testing.T) {
	in := guideInput("observation")
	in.RampElapsedHours = fp(48)
	adv := level(TrustGuide(in), "advisory")
	if !strings.Contains(joined(adv.Waits), "144 h") {
		t.Fatalf("advisory waits = %q, want 144 h left of the 192 h safe ramp", adv.Waits)
	}
	if !strings.Contains(strings.ToLower(joined(adv.Never)), "moderate") {
		t.Fatalf("advisory never = %q, want moderate actions to need approval", adv.Never)
	}
	own := grant(adv, GrantTableOwnership)
	if own.Present == nil || *own.Present || !strings.Contains(own.Detail, "3 of 10") {
		t.Fatalf("ownership grant = %+v, want absent with 3 of 10 tables", own)
	}
	sig := grant(adv, GrantSignalBackend)
	if sig.Present == nil || *sig.Present ||
		!strings.Contains(sig.SQL, "GRANT pg_signal_backend TO sage_agent") {
		t.Fatalf("signal grant = %+v", sig)
	}
}

func TestAutonomousStatesModerateSwitchAndHighRisk(t *testing.T) {
	auto := level(TrustGuide(guideInput("advisory")), "autonomous")
	waits := joined(auto.Waits)
	if !strings.Contains(waits, "trust.tier3_moderate") {
		t.Fatalf("autonomous waits = %q, want the tier3_moderate switch named", waits)
	}
	if !strings.Contains(strings.ToLower(joined(auto.Never)), "high-risk") {
		t.Fatalf("autonomous never = %q, want high-risk always approved", auto.Never)
	}
	if g := grant(auto, GrantAlterSystem); g.Present == nil || *g.Present {
		t.Fatalf("alter system grant = %+v, want absent", g)
	}
	in := guideInput("advisory")
	in.Tier3Moderate = true
	auto = level(TrustGuide(in), "autonomous")
	if strings.Contains(joined(auto.Waits), "trust.tier3_moderate") {
		t.Fatalf("waits %q still name tier3_moderate though it is on", auto.Waits)
	}
}

func TestTrustGuideUnknownGrantsAndRampDone(t *testing.T) {
	in := GuideInput{Current: "autonomous", SafeRampHours: 192, ModerateRampHours: 744,
		Tier3Safe: true, Tier3Moderate: true, RampElapsedHours: fp(1000)}
	levels := TrustGuide(in)
	for _, l := range levels {
		for _, g := range l.Grants {
			if g.Present != nil {
				t.Fatalf("grant %s present=%v without a check", g.Name, *g.Present)
			}
		}
	}
	if w := joined(level(levels, "advisory").Waits); strings.Contains(w, " h left") {
		t.Fatalf("advisory waits %q after the ramp elapsed", w)
	}
	if !level(levels, "autonomous").Current {
		t.Fatal("autonomous not marked current")
	}
}

func TestTrustGuideSafeTierOff(t *testing.T) {
	in := guideInput("observation")
	in.Tier3Safe = false
	adv := level(TrustGuide(in), "advisory")
	if !strings.Contains(joined(adv.Waits), "trust.tier3_safe") {
		t.Fatalf("advisory waits = %q, want the tier3_safe switch named", adv.Waits)
	}
}

func TestTrustGuideRoleNameIsQuoted(t *testing.T) {
	in := guideInput("observation")
	in.Grants.Role = `Sage "Agent"`
	g := grant(level(TrustGuide(in), "observation"), GrantMonitor)
	if !strings.Contains(g.SQL, `"Sage ""Agent"""`) {
		t.Fatalf("grant SQL %q does not quote the role", g.SQL)
	}
}
