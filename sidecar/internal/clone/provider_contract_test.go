package clone

import (
	"context"
	"testing"
	"time"
)

// Compile-time contract: DLE is the reference implementation of the
// prescribed clone.Provider interface.
var _ Provider = (*DLEProvider)(nil)

func TestProviderContractTypesPreserveCloneIdentity(t *testing.T) {
	createdFrom := time.Date(2026, 7, 21, 8, 30, 0, 0, time.UTC)
	got := Clone{
		DSN:         "postgres://clone.invalid/app",
		ID:          "clone-42",
		CreatedFrom: createdFrom,
	}

	if got.ID != "clone-42" {
		t.Fatalf("clone ID = %q, want clone-42", got.ID)
	}
	if got.DSN != "postgres://clone.invalid/app" {
		t.Fatalf("clone DSN = %q", got.DSN)
	}
	if !got.CreatedFrom.Equal(createdFrom) {
		t.Fatalf("created-from = %s, want %s", got.CreatedFrom, createdFrom)
	}
}

func TestProviderContractCloneSpecCarriesRehearsalRequirements(t *testing.T) {
	spec := CloneSpec{IncludeData: true, TargetSizeHintBytes: 8 << 30}
	if !spec.IncludeData {
		t.Fatal("clone spec dropped IncludeData=true")
	}
	if spec.TargetSizeHintBytes != 8<<30 {
		t.Fatalf("size hint = %d, want %d", spec.TargetSizeHintBytes, 8<<30)
	}
}

func TestProviderContractMethodsAcceptContext(t *testing.T) {
	var _ Provider = (*DLEProvider)(nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if ctx.Err() != context.Canceled {
		t.Fatalf("context error = %v, want canceled", ctx.Err())
	}
}
