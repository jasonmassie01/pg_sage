package auth

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Production builds keep the full cost: a real non-test binary
// (testdata/prodcost) hashes at BcryptCost, refuses the test-only minimum
// and still hashes at BcryptCost afterwards. testing.Testing() is true in
// this binary, so only a separate binary can show the production path.
func TestProductionBinary_KeepsFullBcryptCost(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "go", "run", "./testdata/prodcost").CombinedOutput()
	if err != nil {
		t.Fatalf("go run ./testdata/prodcost: %v\n%s", err, out)
	}
	want := fmt.Sprintf("cost=%d\nrefused=true\ncost_after=%d\n", BcryptCost, BcryptCost)
	if got := strings.ReplaceAll(string(out), "\r\n", "\n"); got != want {
		t.Fatalf("production binary reported\n%s\nwant\n%s", got, want)
	}
}

// The refusal in-process: with the test-binary check reporting false, the
// hook panics and the cost stays at BcryptCost.
func TestUseMinimumHashCostForTests_RefusedOutsideATestBinary(t *testing.T) {
	saved := inTestBinary
	inTestBinary = func() bool { return false }
	defer func() { inTestBinary = saved }()
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		UseMinimumHashCostForTests()
	}()
	msg, _ := recovered.(string)
	if !strings.Contains(msg, "outside a test binary") {
		t.Fatalf("recovered %v, want the outside-a-test-binary panic", recovered)
	}
	if got := int(hashCost.Load()); got != BcryptCost {
		t.Fatalf("cost after the refusal = %d, want BcryptCost %d", got, BcryptCost)
	}
}
