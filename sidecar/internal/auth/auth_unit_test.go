package auth

import (
	"context"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// HashPassword edge cases
// ---------------------------------------------------------------------------

func TestHashPassword_EmptyPassword(t *testing.T) {
	hash, err := HashPassword("")
	if err != nil {
		t.Fatalf("HashPassword empty returned error: %v", err)
	}
	if hash == "" {
		t.Fatal("hash of empty password should not be empty string")
	}
	// The hash must still verify against the empty string.
	if !CheckPassword(hash, "") {
		t.Error("hash of empty password does not verify against empty string")
	}
	// And must NOT verify against a non-empty string.
	if CheckPassword(hash, "notempty") {
		t.Error("hash of empty password should not verify against 'notempty'")
	}
}

func TestHashPassword_LongPassword_BcryptTruncation(t *testing.T) {
	// bcrypt silently truncates input to 72 bytes. Two passwords that share
	// the first 72 bytes but differ after that will produce the same hash.
	// This test documents that known limitation.
	base := strings.Repeat("A", 72)
	pw72 := base           // exactly 72 bytes
	pw80 := base + "EXTRA" // 77 bytes -- extra bytes are truncated

	hash, err := HashPassword(pw72)
	if err != nil {
		t.Fatalf("HashPassword(72-byte) error: %v", err)
	}

	// Both should match because bcrypt truncates at 72.
	if !CheckPassword(hash, pw72) {
		t.Error("72-byte password should verify against its own hash")
	}
	if !CheckPassword(hash, pw80) {
		t.Error("80-byte password should also match due to bcrypt 72-byte truncation")
	}
}

func TestHashPassword_UnicodeCharacters(t *testing.T) {
	passwords := []string{
		"\u00e9\u00e8\u00ea\u00eb",                 // French accented chars
		"\u4f60\u597d\u4e16\u754c",                 // Chinese characters
		"\U0001f512\U0001f511\U0001f513",           // Emoji: lock, key, unlock
		"p\u00e4\u00df\u0175\u00f6rd",              // Mixed Latin + diacritics
		"\u0000null\u0000embedded",                  // Embedded null bytes
	}
	for _, pw := range passwords {
		t.Run(pw, func(t *testing.T) {
			hash, err := HashPassword(pw)
			if err != nil {
				t.Fatalf("HashPassword(%q) error: %v", pw, err)
			}
			if hash == "" {
				t.Fatalf("hash is empty for password %q", pw)
			}
			if !CheckPassword(hash, pw) {
				t.Errorf("password %q does not verify against its own hash", pw)
			}
			// Mutated version must NOT match.
			if CheckPassword(hash, pw+"x") {
				t.Errorf("password %q+x should not match hash of %q", pw, pw)
			}
		})
	}
}

func TestHashPassword_ProducesDifferentHashesForSameInput(t *testing.T) {
	// bcrypt includes a random salt, so two hashes of the same password
	// must differ.
	h1, err := HashPassword("samepassword")
	if err != nil {
		t.Fatalf("first HashPassword error: %v", err)
	}
	h2, err := HashPassword("samepassword")
	if err != nil {
		t.Fatalf("second HashPassword error: %v", err)
	}
	if h1 == h2 {
		t.Error("two bcrypt hashes of the same password should differ (unique salts)")
	}
	// Both must still verify.
	if !CheckPassword(h1, "samepassword") {
		t.Error("h1 should verify")
	}
	if !CheckPassword(h2, "samepassword") {
		t.Error("h2 should verify")
	}
}

// ---------------------------------------------------------------------------
// CheckPassword edge cases
// ---------------------------------------------------------------------------

func TestCheckPassword_CorruptedHash(t *testing.T) {
	hash, err := HashPassword("test")
	if err != nil {
		t.Fatalf("HashPassword error: %v", err)
	}

	// Truncate the hash to corrupt it.
	truncated := hash[:10]
	if CheckPassword(truncated, "test") {
		t.Error("truncated hash should not verify")
	}

	// Flip a character in the middle.
	corrupted := []byte(hash)
	corrupted[20] ^= 0xFF
	if CheckPassword(string(corrupted), "test") {
		t.Error("corrupted hash should not verify")
	}
}

func TestCheckPassword_EmptyHashEmptyPassword(t *testing.T) {
	// Empty hash with empty password should NOT match -- there is no valid
	// bcrypt hash that is an empty string.
	if CheckPassword("", "") {
		t.Error("empty hash + empty password should return false")
	}
}

func TestCheckPassword_CaseSensitivity(t *testing.T) {
	hash, err := HashPassword("Secret")
	if err != nil {
		t.Fatalf("HashPassword error: %v", err)
	}
	if CheckPassword(hash, "secret") {
		t.Error("password check should be case-sensitive: 'secret' != 'Secret'")
	}
	if CheckPassword(hash, "SECRET") {
		t.Error("password check should be case-sensitive: 'SECRET' != 'Secret'")
	}
	if !CheckPassword(hash, "Secret") {
		t.Error("exact case should match")
	}
}

func TestCheckPassword_GarbageHash(t *testing.T) {
	// Completely invalid bcrypt strings.
	garbage := []string{
		"notahash",
		"$2a$12$",                   // valid prefix but no payload
		"$2a$12$short",              // too short
		"$$$$$$",                    // nonsense
		"$2a$99$" + strings.Repeat("a", 53), // invalid cost
	}
	for _, h := range garbage {
		if CheckPassword(h, "anything") {
			t.Errorf("garbage hash %q should not verify", h)
		}
	}
}

// ---------------------------------------------------------------------------
// IsValidRole completeness
// ---------------------------------------------------------------------------

func TestIsValidRole_AllDefinedConstants(t *testing.T) {
	// Verify every role constant is accepted.
	constants := []struct {
		name  string
		value string
	}{
		{"RoleAdmin", RoleAdmin},
		{"RoleOperator", RoleOperator},
		{"RoleViewer", RoleViewer},
	}
	for _, c := range constants {
		if !IsValidRole(c.value) {
			t.Errorf("IsValidRole(%s=%q) = false, want true", c.name, c.value)
		}
	}
}

func TestIsValidRole_InvalidInputs(t *testing.T) {
	tests := []struct {
		input string
	}{
		{""},
		{" "},
		{" admin"},
		{"admin "},
		{" admin "},
		{"Admin"},
		{"ADMIN"},
		{"Operator"},
		{"VIEWER"},
		{"superadmin"},
		{"root"},
		{"viewer\n"},
		{"viewer\t"},
		{"viewer\x00"},
	}
	for _, tc := range tests {
		if IsValidRole(tc.input) {
			t.Errorf("IsValidRole(%q) = true, want false", tc.input)
		}
	}
}

func TestIsValidRole_ValidRolesMapCompleteness(t *testing.T) {
	// The ValidRoles map should have exactly 3 entries.
	if got := len(ValidRoles); got != 3 {
		t.Errorf("len(ValidRoles) = %d, want 3", got)
	}
	// Every key in ValidRoles must map to true.
	for role, v := range ValidRoles {
		if !v {
			t.Errorf("ValidRoles[%q] = false, want true", role)
		}
	}
}

// ---------------------------------------------------------------------------
// SessionDuration constant
// ---------------------------------------------------------------------------

func TestSessionDuration_Is24Hours(t *testing.T) {
	expected := 24 * time.Hour
	if SessionDuration != expected {
		t.Errorf("SessionDuration = %v, want %v", SessionDuration, expected)
	}
}

func TestBcryptCost_Value(t *testing.T) {
	if BcryptCost != 12 {
		t.Errorf("BcryptCost = %d, want 12", BcryptCost)
	}
}

// ---------------------------------------------------------------------------
// User struct zero value behavior
// ---------------------------------------------------------------------------

func TestUser_ZeroValue(t *testing.T) {
	var u User
	if u.ID != 0 {
		t.Errorf("zero User.ID = %d, want 0", u.ID)
	}
	if u.Email != "" {
		t.Errorf("zero User.Email = %q, want empty", u.Email)
	}
	if u.Role != "" {
		t.Errorf("zero User.Role = %q, want empty", u.Role)
	}
	if !u.CreatedAt.IsZero() {
		t.Errorf("zero User.CreatedAt = %v, want zero time", u.CreatedAt)
	}
	if u.LastLogin != nil {
		t.Errorf("zero User.LastLogin = %v, want nil", u.LastLogin)
	}
}

func TestUser_ZeroRoleIsInvalid(t *testing.T) {
	var u User
	if IsValidRole(u.Role) {
		t.Error("zero-value role (empty string) should not be valid")
	}
}

// ---------------------------------------------------------------------------
// Session struct zero value behavior
// ---------------------------------------------------------------------------

func TestSession_ZeroValue(t *testing.T) {
	var s Session
	if s.ID != "" {
		t.Errorf("zero Session.ID = %q, want empty", s.ID)
	}
	if s.UserID != 0 {
		t.Errorf("zero Session.UserID = %d, want 0", s.UserID)
	}
	if !s.ExpiresAt.IsZero() {
		t.Errorf("zero Session.ExpiresAt = %v, want zero time", s.ExpiresAt)
	}
}

// ---------------------------------------------------------------------------
// StartSessionCleaner: context cancellation
// ---------------------------------------------------------------------------

func TestStartSessionCleaner_RespectsContextCancellation(t *testing.T) {
	// StartSessionCleaner requires a *pgxpool.Pool, but the goroutine will
	// exit on ctx.Done() before the ticker fires if we set a long interval.
	// We pass nil pool -- the function will never call CleanExpiredSessions
	// because the context is cancelled before the first tick.
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		// Use a very long interval so the ticker never fires.
		StartSessionCleaner(ctx, nil, 1*time.Hour)
		close(done)
	}()

	// Cancel immediately.
	cancel()

	select {
	case <-done:
		// Goroutine exited -- success.
	case <-time.After(2 * time.Second):
		t.Fatal("StartSessionCleaner did not exit within 2s after context cancellation")
	}
}

func TestStartSessionCleaner_ExitsMidRun(t *testing.T) {
	// Verify that even after one or more ticks, the cleaner still exits
	// promptly when the context is cancelled. We use a very short interval
	// so at least one tick fires (with nil pool it will log a warning but
	// not panic -- CleanExpiredSessions calls pool.Exec which will panic
	// on nil pool). To avoid that, we cancel before the first tick.
	//
	// Strategy: set interval to 50ms, wait 10ms, then cancel. The first
	// tick hasn't fired yet, so no nil-pool dereference occurs.
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		StartSessionCleaner(ctx, nil, 50*time.Millisecond)
		close(done)
	}()

	// Cancel before the first tick at 50ms.
	time.Sleep(10 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// Success.
	case <-time.After(2 * time.Second):
		t.Fatal("StartSessionCleaner did not exit within 2s after mid-run cancellation")
	}
}

func TestStartSessionCleaner_ImmediateCancelBeforeFirstTick(t *testing.T) {
	// Cancel the context BEFORE starting the cleaner. The goroutine should
	// return immediately on the first select iteration.
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled

	done := make(chan struct{})
	go func() {
		StartSessionCleaner(ctx, nil, 1*time.Hour)
		close(done)
	}()

	select {
	case <-done:
		// Success.
	case <-time.After(2 * time.Second):
		t.Fatal("StartSessionCleaner did not exit with pre-cancelled context")
	}
}

// ---------------------------------------------------------------------------
// Role constants: verify values haven't drifted
// ---------------------------------------------------------------------------

func TestRoleConstants_Values(t *testing.T) {
	tests := []struct {
		name     string
		got      string
		expected string
	}{
		{"RoleAdmin", RoleAdmin, "admin"},
		{"RoleOperator", RoleOperator, "operator"},
		{"RoleViewer", RoleViewer, "viewer"},
	}
	for _, tc := range tests {
		if tc.got != tc.expected {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.expected)
		}
	}
}
