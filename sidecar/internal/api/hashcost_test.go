package api

import (
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/pg-sage/sidecar/internal/auth"
)

// TestMain lowers the password hash cost: under the race detector each
// cost-12 hash or check takes seconds, and this suite creates and logs in
// many users (the package outran CI's 15-minute -race budget).
func TestMain_HashesPasswordsAtTheMinimumCost(t *testing.T) {
	h, err := auth.HashPassword("pw-api-suite")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	cost, err := bcrypt.Cost([]byte(h))
	if err != nil || cost != bcrypt.MinCost {
		t.Fatalf("hash cost = %d (%v), want bcrypt.MinCost %d", cost, err, bcrypt.MinCost)
	}
	if !auth.CheckPassword(h, "pw-api-suite") {
		t.Fatal("a minimum-cost hash does not check")
	}
}
