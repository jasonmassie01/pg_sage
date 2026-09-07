package logwatch

import "testing"

func TestTruncate_NoTruncation(t *testing.T) {
	s := "hello world"
	got := Truncate(s, 50)
	if got != s {
		t.Fatalf("expected %q, got %q", s, got)
	}
}

func TestTruncate_ExactBoundary(t *testing.T) {
	s := "abcde"
	got := Truncate(s, 5)
	if got != s {
		t.Fatalf("expected %q, got %q", s, got)
	}
}

func TestTruncate_OverBoundary(t *testing.T) {
	s := "abcdefghij"
	got := Truncate(s, 5)
	if len(got) > 5 {
		t.Fatalf("expected len <= 5, got %d (%q)", len(got), got)
	}
	if got != "abcde" {
		t.Fatalf("expected %q, got %q", "abcde", got)
	}
}

func TestTruncate_EmptyString(t *testing.T) {
	got := Truncate("", 10)
	if got != "" {
		t.Fatalf("expected empty string, got %q", got)
	}
}

func TestTruncate_MultibyteSafe(t *testing.T) {
	// Each rune here is 3 bytes (U+4E16, U+754C, U+4F60, U+597D).
	// "世界你好" = 12 bytes.
	s := "世界你好"
	// maxLen=7 falls between rune boundaries (6 and 9).
	// Should truncate to 6 bytes ("世界"), not split mid-rune.
	got := Truncate(s, 7)
	if got != "世界" {
		t.Fatalf("expected %q (6 bytes), got %q (%d bytes)", "世界", got, len(got))
	}
}

func TestTruncate_MultibyteSafe_ExactRuneBoundary(t *testing.T) {
	s := "世界你好" // 12 bytes, 4 runes of 3 bytes each
	// maxLen=6 is exactly at a rune boundary.
	got := Truncate(s, 6)
	if got != "世界" {
		t.Fatalf("expected %q, got %q", "世界", got)
	}
}

func TestTruncate_ZeroMaxLen(t *testing.T) {
	got := Truncate("hello", 0)
	if got != "" {
		t.Fatalf("expected empty string for maxLen=0, got %q", got)
	}
}

func TestTruncate_SingleByte(t *testing.T) {
	got := Truncate("hello", 1)
	if got != "h" {
		t.Fatalf("expected %q, got %q", "h", got)
	}
}

func TestConstants(t *testing.T) {
	if MaxRawMessageLen != 500 {
		t.Fatalf("MaxRawMessageLen = %d, want 500", MaxRawMessageLen)
	}
	if MaxQueryLen != 200 {
		t.Fatalf("MaxQueryLen = %d, want 200", MaxQueryLen)
	}
	if MaxAffectedPIDs != 20 {
		t.Fatalf("MaxAffectedPIDs = %d, want 20", MaxAffectedPIDs)
	}
}
