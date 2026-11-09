package notify

import (
	"errors"
	"testing"

	"github.com/pg-sage/sidecar/internal/egress"
)

func TestTargetPolicyUsesEgressFloor(t *testing.T) {
	for _, p := range []TargetPolicy{{}, {AllowPrivate: true}} {
		for _, address := range []string{
			"169.254.169.254:80", "[fd00:ec2::254]:80", "224.0.0.1:25",
		} {
			err := p.dialControl("tcp", address, nil)
			if !errors.Is(err, ErrTargetBlocked) || !errors.Is(err, egress.ErrBlocked) {
				t.Errorf("AllowPrivate=%v dial %s = %v, want both sentinels",
					p.AllowPrivate, address, err)
			}
		}
		if err := p.ValidateHost("fd00:ec2::254"); !errors.Is(err, egress.ErrBlocked) {
			t.Errorf("AllowPrivate=%v ValidateHost(IMDS v6) = %v", p.AllowPrivate, err)
		}
	}
}

func TestTargetPolicyKeepsStricterPrivateDefault(t *testing.T) {
	strict := TargetPolicy{}
	for _, address := range []string{"10.0.0.1:443", "127.0.0.1:25", "100.64.0.1:443"} {
		err := strict.dialControl("tcp", address, nil)
		if !errors.Is(err, ErrTargetBlocked) || errors.Is(err, egress.ErrBlocked) {
			t.Errorf("strict dial %s = %v, want policy (not floor) refusal", address, err)
		}
	}
	relaxed := TargetPolicy{AllowPrivate: true}
	if err := relaxed.dialControl("tcp", "10.0.0.1:587", nil); err != nil {
		t.Fatalf("AllowPrivate relay refused: %v", err)
	}
}
