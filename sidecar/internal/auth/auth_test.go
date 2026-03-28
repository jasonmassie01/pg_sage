package auth

import (
	"testing"
)

func TestHashPassword(t *testing.T) {
	hash, err := HashPassword("secret123")
	if err != nil {
		t.Fatalf("HashPassword error: %v", err)
	}
	if hash == "" {
		t.Fatal("hash is empty")
	}
	if hash == "secret123" {
		t.Fatal("hash equals plaintext")
	}
}

func TestCheckPassword_Valid(t *testing.T) {
	hash, err := HashPassword("mypassword")
	if err != nil {
		t.Fatalf("HashPassword error: %v", err)
	}
	if !CheckPassword(hash, "mypassword") {
		t.Error("expected password to match")
	}
}

func TestCheckPassword_Invalid(t *testing.T) {
	hash, err := HashPassword("mypassword")
	if err != nil {
		t.Fatalf("HashPassword error: %v", err)
	}
	if CheckPassword(hash, "wrongpassword") {
		t.Error("expected password to not match")
	}
}

func TestCheckPassword_EmptyHash(t *testing.T) {
	if CheckPassword("", "anything") {
		t.Error("expected false for empty hash")
	}
}

func TestIsValidRole(t *testing.T) {
	tests := []struct {
		role  string
		valid bool
	}{
		{"admin", true},
		{"operator", true},
		{"viewer", true},
		{"superadmin", false},
		{"", false},
		{"Admin", false},
	}
	for _, tc := range tests {
		if got := IsValidRole(tc.role); got != tc.valid {
			t.Errorf("IsValidRole(%q) = %v, want %v",
				tc.role, got, tc.valid)
		}
	}
}
