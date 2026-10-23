// Command prodcost is a production (non-test) binary for
// TestProductionBinary_KeepsFullBcryptCost: it reports the cost of a new
// password hash, whether the test-only minimum cost was refused, and the
// cost of a hash made after the attempt.
package main

import (
	"fmt"
	"os"

	"golang.org/x/crypto/bcrypt"

	"github.com/pg-sage/sidecar/internal/auth"
)

func costOfNewHash() int {
	h, err := auth.HashPassword("pw-production-binary")
	if err != nil {
		fmt.Fprintln(os.Stderr, "hash:", err)
		os.Exit(2)
	}
	cost, err := bcrypt.Cost([]byte(h))
	if err != nil {
		fmt.Fprintln(os.Stderr, "cost:", err)
		os.Exit(2)
	}
	return cost
}

// tryLowering reports whether UseMinimumHashCostForTests refused (panicked).
func tryLowering() (refused bool) {
	defer func() { refused = recover() != nil }()
	auth.UseMinimumHashCostForTests()
	return false
}

func main() {
	fmt.Printf("cost=%d\n", costOfNewHash())
	fmt.Printf("refused=%v\n", tryLowering())
	fmt.Printf("cost_after=%d\n", costOfNewHash())
}
