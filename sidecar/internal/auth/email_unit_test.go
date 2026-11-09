package auth

import (
	"errors"
	"testing"
)

// apiauth-05: one email identity, whatever its case or surrounding
// whitespace. No concurrent access tests here: both functions are pure.

func TestNormalizeEmail(t *testing.T) {
	cases := map[string]string{
		"alice@corp.com":          "alice@corp.com",
		"Alice@Corp.COM":          "alice@corp.com",
		"  ALICE@corp.com\t\n":    "alice@corp.com",
		"":                        "",
		"   ":                     "",
		"Bob.O'Neil@Example.Org ": "bob.o'neil@example.org",
	}
	for in, want := range cases {
		if got := NormalizeEmail(in); got != want {
			t.Errorf("NormalizeEmail(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidateEmail_AcceptsBareAddressesNormalized(t *testing.T) {
	cases := map[string]string{
		"BOB@x.io":                 "bob@x.io",
		"  carol.d+ops@Corp.com  ": "carol.d+ops@corp.com",
		"a@b":                      "a@b",
	}
	for in, want := range cases {
		got, err := ValidateEmail(in)
		if err != nil || got != want {
			t.Errorf("ValidateEmail(%q) = (%q, %v), want (%q, nil)", in, got, err, want)
		}
	}
}

func TestValidateEmail_RejectsMalformed(t *testing.T) {
	for _, in := range []string{
		"", "   ", "not-an-email", "@corp.com", "alice@",
		"Alice <alice@corp.com>", "alice@corp.com, bob@corp.com",
		"alice @corp.com", "alice@corp.com\nBcc: eve@evil.io",
	} {
		got, err := ValidateEmail(in)
		if !errors.Is(err, ErrInvalidEmail) {
			t.Errorf("ValidateEmail(%q) = (%q, %v), want ErrInvalidEmail", in, got, err)
		}
		if got != "" {
			t.Errorf("ValidateEmail(%q) returned %q with an error, want empty", in, got)
		}
	}
}
