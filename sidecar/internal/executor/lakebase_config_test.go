package executor

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestLakebaseBlocksInstanceSettingsEvenWithAnAdapter(t *testing.T) {
	adapter := &managedConfigAdapterStub{result: ManagedConfigResult{InEffect: true}}
	exec := newManagedConfigTestExecutor("lakebase")
	exec.WithManagedConfigAdapter(adapter)
	result, handled, err := exec.applyManagedCustodianConfig(context.Background(), managedWALProposal("10GB"))
	if !handled || result.InEffect || !errors.Is(err, ErrManagedConfigUnsupported) || adapter.calls != 0 {
		t.Fatalf("Lakebase instance setting was not refused: handled=%v result=%+v error=%v calls=%d", handled, result, err, adapter.calls)
	}
	if !isManagedProvider("lakebase") || !strings.Contains(managedConfigGuidance("lakebase", "shared_buffers"), "unavailable") {
		t.Fatal("Lakebase should explain that instance settings are provider controlled")
	}
}
