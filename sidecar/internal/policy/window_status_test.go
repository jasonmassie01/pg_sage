package policy

import (
	"context"
	"errors"
	"testing"
	"time"
)

func windowReporter(t *testing.T, fixture gateFixture) WindowReporter {
	t.Helper()
	reporter, ok := newTestGate(t, fixture).(WindowReporter)
	if !ok {
		t.Fatal("standing gate does not report maintenance windows")
	}
	return reporter
}

func TestMaintenanceWindowOpenMatchesModerateGate(t *testing.T) {
	sundayTwoAM := time.Date(2026, 7, 26, 2, 0, 0, 0, time.UTC)
	mondayNoon := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name        string
		windows     []string
		now         time.Time
		trustWindow bool
		want        bool
		wantErr     bool
	}{
		{"both windows open", []string{"0 2 * * 0"}, sundayTwoAM, true, true, false},
		{"policy window closed", []string{"0 2 * * 0"}, mondayNoon, true, false, false},
		{"trust window closed", []string{"always"}, sundayTwoAM, false, false, false},
		// An invalid policy (no windows) fails closed, as the gate does.
		{"no policy windows", nil, sundayTwoAM, true, false, true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			doc := UnattendedProfile()
			doc.MaintenanceWindows = test.windows
			fixture := gateFixture{policy: doc, now: test.now}
			reporter := windowReporter(t, fixture)
			if !test.trustWindow {
				fixture.runtimeSet = true
				fixture.runtime = RuntimeState{
					TrustLevel: TrustAutonomous, InConfiguredWindow: false,
				}
				reporter = windowReporter(t, fixture)
			}
			req := validIndexRequest()
			req.Contract.RiskTier = RiskModerate
			open, err := reporter.MaintenanceWindowOpen(context.Background(), req)
			if (err != nil) != test.wantErr || open != test.want {
				t.Fatalf("window open = %v, %v; want %v (error %v)",
					open, err, test.want, test.wantErr)
			}
			// The reporter must agree with the gate's own window verdict.
			gate := newTestGate(t, fixture)
			decision := gate.Authorize(context.Background(), req)
			blocked := decision.Reason == ReasonOutsideMaintenanceWindow
			if test.trustWindow && !test.wantErr && blocked == test.want {
				t.Fatalf("reporter=%v but gate decision %+v", open, decision)
			}
		})
	}
}

func TestMaintenanceWindowOpenFailsClosedWithoutPolicy(t *testing.T) {
	want := errors.New("policy store down")
	reporter := windowReporter(t, gateFixture{policyErr: want})
	open, err := reporter.MaintenanceWindowOpen(context.Background(), validIndexRequest())
	if open || !errors.Is(err, want) {
		t.Fatalf("policy failure = %v, %v; want closed with error", open, err)
	}
}
