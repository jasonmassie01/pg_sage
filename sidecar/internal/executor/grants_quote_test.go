package executor

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/onboarding"
)

// A role whose name needs quoting (upper case, spaces) must be quoted in
// the SQL the startup check prints, exactly as the Grant more guide quotes
// it; unquoted, the GRANT would name a different, case-folded role.

func TestGrantFixesQuoteTheRole(t *testing.T) {
	if got := schemaCreateGrant("Sage Agent", publicLacking).fix; got !=
		`GRANT CREATE ON SCHEMA public TO "Sage Agent"` {
		t.Fatalf("schema CREATE fix = %q, want the role quoted", got)
	}
	if got := signalBackendGrant("Sage Agent").fix; got !=
		`GRANT pg_signal_backend TO "Sage Agent"` {
		t.Fatalf("pg_signal_backend fix = %q, want the role quoted", got)
	}
	if got := signalBackendGrant("sage_agent").fix; got != "GRANT pg_signal_backend TO sage_agent" {
		t.Fatalf("plain role fix = %q, want it unquoted", got)
	}
}

// Integration: for a mixed-case role, both grants the startup check warns
// about carry the same SQL as the guide.
func TestVerifyGrantsAgreesWithGrantMoreQuotedRole(t *testing.T) {
	name := fmt.Sprintf("Grants_%06X", time.Now().UnixNano()&0xffffff)
	pool, role := grantsRoleNamed(t, tablesInApp(t), name)
	quoted := `"` + role + `"`
	g, err := onboarding.CheckGrants(t.Context(), pool)
	if err != nil {
		t.Fatalf("onboarding grants: %v", err)
	}
	var guide []string
	for _, l := range onboarding.TrustGuide(onboarding.GuideInput{Grants: g}) {
		if l.Level != "advisory" {
			continue
		}
		for _, gr := range l.Grants {
			if gr.Name == onboarding.GrantSchemaCreate || gr.Name == onboarding.GrantSignalBackend {
				guide = append(guide, strings.TrimSuffix(gr.SQL, ";"))
			}
		}
	}
	want := []string{"GRANT CREATE ON SCHEMA app TO " + quoted,
		"GRANT pg_signal_backend TO " + quoted}
	if strings.Join(guide, "|") != strings.Join(want, "|") {
		t.Fatalf("guide SQL = %q, want %q", guide, want)
	}
	var lines []logLine
	VerifyGrants(t.Context(), pool, "ignored", "advisory", captureLog(&lines))
	if len(lines) != 2 || !strings.HasSuffix(lines[0].text, want[0]) ||
		!strings.HasSuffix(lines[1].text, want[1]) {
		t.Fatalf("startup lines = %+v, want the fixes %q", lines, want)
	}
}
