package earned

import (
	"encoding/json"
	"testing"
)

// Autonomy levels (AI-SRE-SPEC §7.3): L0 evidence only, L1 proposal,
// L2 one-click approval handoff, L3 auto-execute reversible bounded
// actions in the window. L4 is reserved and can never be granted.

func TestParseLevelAcceptsLabelsAndDigits(t *testing.T) {
	for in, want := range map[string]Level{"L0": L0, "l1": L1, " L2 ": L2, "3": L3,
		"L4": L4} {
		got, err := ParseLevel(in)
		if err != nil || got != want {
			t.Errorf("ParseLevel(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "L5", "-1", "L-1", "two", "L", "L2x", "22"} {
		if got, err := ParseLevel(in); err == nil {
			t.Errorf("ParseLevel(%q) = %v, want an error", in, got)
		}
	}
}

func TestLevelStringAndGrantable(t *testing.T) {
	cases := []struct {
		level     Level
		label     string
		grantable bool
	}{
		{L0, "L0", true}, {L1, "L1", true}, {L2, "L2", true}, {L3, "L3", true},
		{L4, "L4", false}, {Level(-1), "L?", false}, {Level(9), "L?", false},
	}
	for _, c := range cases {
		if c.level.String() != c.label || c.level.Grantable() != c.grantable {
			t.Errorf("%d: String=%q Grantable=%v; want %q %v", int(c.level),
				c.level.String(), c.level.Grantable(), c.label, c.grantable)
		}
	}
	if MaxGrantable != L3 {
		t.Fatalf("MaxGrantable = %v, want L3 (L4 is reserved)", MaxGrantable)
	}
}

func TestLevelJSONIsTheLabel(t *testing.T) {
	raw, err := json.Marshal(struct{ L Level }{L2})
	if err != nil || string(raw) != `{"L":"L2"}` {
		t.Fatalf("marshal = %s, %v", raw, err)
	}
	var back struct{ L Level }
	if err := json.Unmarshal([]byte(`{"L":"L3"}`), &back); err != nil || back.L != L3 {
		t.Fatalf("unmarshal = %v, %v", back.L, err)
	}
	if err := json.Unmarshal([]byte(`{"L":"L7"}`), &back); err == nil {
		t.Fatal("unmarshal accepted L7")
	}
}

func TestMinLevel(t *testing.T) {
	if got := MinLevel(L3, L1, L2); got != L1 {
		t.Fatalf("MinLevel = %v, want L1", got)
	}
	if got := MinLevel(); got != L0 {
		t.Fatalf("MinLevel() = %v, want L0 (no grant is no autonomy)", got)
	}
}
