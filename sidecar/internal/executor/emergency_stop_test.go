package executor

import (
	"context"
	"strings"
	"testing"
)

// D8: every durable stop/resume names who made it; an anonymous write
// would leave the audit trail unable to answer "who pulled the brake".
func TestSetEmergencyStopRequiresActor(t *testing.T) {
	err := SetEmergencyStop(context.Background(), nil, true, "")
	if err == nil || !strings.Contains(err.Error(), "actor is required") {
		t.Fatalf("error = %v, want actor is required", err)
	}
}

func TestReadEmergencyStopWithoutPoolIsAnError(t *testing.T) {
	state, err := ReadEmergencyStop(context.Background(), nil)
	if err == nil || state.Stopped || state.UpdatedBy != "" {
		t.Fatalf("state=%+v err=%v, want zero state and an error", state, err)
	}
}
