package logwatch

import (
	"strings"
	"testing"
)

func TestLCMessagesWarning_EnglishAndCAreAccepted(t *testing.T) {
	for _, v := range []string{"C", "POSIX", "C.UTF-8", "c.utf8", "en_US.UTF-8",
		"en_US.utf8", "en", "en_GB", "English_United States.1252"} {
		if got := LCMessagesWarning(v); got != "" {
			t.Errorf("LCMessagesWarning(%q) = %q, want no warning", v, got)
		}
	}
}

func TestLCMessagesWarning_OtherLanguagesWarn(t *testing.T) {
	for _, v := range []string{"de_DE.UTF-8", "fr_FR", "ja_JP.eucJP", "ru_RU.UTF-8",
		"enx", "Encoding"} {
		got := LCMessagesWarning(v)
		if got == "" {
			t.Errorf("LCMessagesWarning(%q) = no warning, want one", v)
			continue
		}
		if !strings.Contains(got, "lc_messages") || !strings.Contains(got, v) {
			t.Errorf("LCMessagesWarning(%q) = %q, want the setting and its value", v, got)
		}
	}
}

// An empty lc_messages inherits the server environment, which pg_sage
// cannot see, so it warns rather than assuming English.
func TestLCMessagesWarning_EmptyWarns(t *testing.T) {
	got := LCMessagesWarning("")
	if !strings.Contains(got, "lc_messages") || !strings.Contains(got, "empty") {
		t.Fatalf("LCMessagesWarning(\"\") = %q, want an empty-value warning", got)
	}
}
