package candidate

import (
	"errors"
	"testing"
)

func TestTransition_FromProposedOnly(t *testing.T) {
	all := []Status{StatusProposed, StatusPlanned, StatusRejected, StatusSuperseded}
	allowed := map[[2]Status]bool{
		{StatusProposed, StatusPlanned}:    true,
		{StatusProposed, StatusRejected}:   true,
		{StatusProposed, StatusSuperseded}: true,
	}
	for _, from := range all {
		for _, to := range all {
			err := Transition(from, to)
			if allowed[[2]Status{from, to}] {
				if err != nil {
					t.Errorf("%s -> %s refused: %v", from, to, err)
				}
				continue
			}
			if !errors.Is(err, ErrTransition) {
				t.Errorf("%s -> %s: err = %v, want ErrTransition", from, to, err)
			}
		}
	}
}

func TestTransition_SupersededNeverBecomesPlanned(t *testing.T) {
	err := Transition(StatusSuperseded, StatusPlanned)
	if !errors.Is(err, ErrTransition) {
		t.Fatalf("superseded -> planned: err = %v", err)
	}
}

func TestTransition_UnknownStatusIsRefused(t *testing.T) {
	for _, pair := range [][2]Status{{"", StatusPlanned}, {StatusProposed, "done"}} {
		if err := Transition(pair[0], pair[1]); !errors.Is(err, ErrTransition) {
			t.Errorf("%q -> %q: err = %v, want ErrTransition", pair[0], pair[1], err)
		}
	}
}
