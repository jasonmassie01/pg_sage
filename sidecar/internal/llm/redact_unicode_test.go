package llm

import (
	"strings"
	"testing"
)

// Phase 0 #3: redactQueryValue took offsets from strings.ToLower(value) and
// applied them to value. Length-changing runes before the key moved the
// cut, which leaked the secret or panicked.
func TestRedactQueryValueLengthChangingRunes(t *testing.T) {
	cases := []struct{ in, key string }{
		{strings.Repeat("K", 8) + " key=SECRET1&x=1", "key"},
		{strings.Repeat("İ", 8) + " token=SECRET1&x=1", "token"},
		{"KİK https://h/p?access_token=SECRET1", "access_token"},
		{strings.Repeat("İ", 30) + "KEY=SECRET1", "key"},
		{"Signature=SECRET1 " + strings.Repeat("K", 5) + "signature=SECRET1", "signature"},
	}
	for _, c := range cases {
		var got string
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("redactQueryValue(%q) panicked: %v", c.in, r)
				}
			}()
			got = redactQueryValue(c.in, c.key)
		}()
		if strings.Contains(got, "SECRET1") {
			t.Errorf("redactQueryValue(%q, %q) leaked the secret: %q", c.in, c.key, got)
		}
		if !strings.Contains(got, "[REDACTED]") {
			t.Errorf("redactQueryValue(%q, %q) = %q, want a [REDACTED] marker", c.in, c.key, got)
		}
	}
}

func TestRedactQueryValueStopsAtDelimitersAndKeepsText(t *testing.T) {
	got := redactQueryValue(`GET https://api/x?Key=abc&model=m "next"`, "key")
	want := `GET https://api/x?Key=[REDACTED]&model=m "next"`
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
	if got := redactQueryValue("no secrets here", "key"); got != "no secrets here" {
		t.Errorf("text without the key changed: %q", got)
	}
	if got := redactQueryValue("", "key"); got != "" {
		t.Errorf("empty input changed: %q", got)
	}
	if got := redactQueryValue("key=", "key"); got != "key=[REDACTED]" {
		t.Errorf("empty value: got %q", got)
	}
}

// The Kelvin sign folds to "k" under Unicode case folding, but a provider
// never treats "Key=" as the key parameter. Only ASCII case variants
// are keys; the non-ASCII look-alike is left as text.
func TestRedactQueryValueMatchesASCIICaseOnly(t *testing.T) {
	in := "Key=visible&KEY=hidden"
	got := redactQueryValue(in, "key")
	if !strings.Contains(got, "Key=visible") || strings.Contains(got, "hidden") {
		t.Errorf("redactQueryValue(%q) = %q", in, got)
	}
}

func TestRedactProviderTextRedactsEveryKey(t *testing.T) {
	in := "Kİ GET /v1?api_key=A1&token=B2&access_token=C3&signature=D4&key=E5"
	got := redactProviderText(in)
	for _, secret := range []string{"A1", "B2", "C3", "D4", "E5"} {
		if strings.Contains(got, secret) {
			t.Errorf("redactProviderText leaked %s: %q", secret, got)
		}
	}
}

// FuzzRedactQueryValue: never panics, and every ASCII-case occurrence of
// "token=" in the output is followed by the redaction marker.
func FuzzRedactQueryValue(f *testing.F) {
	for _, seed := range []string{
		"", "token=", "TOKEN=abc", "Ktoken=abc", "İİtoken=x&y",
		"token=token=x", "\xff token=\xfe", "tokKen=abc",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, in string) {
		out := redactQueryValue(in, "token")
		folded := asciiLower(out)
		for i := 0; ; {
			idx := strings.Index(folded[i:], "token=")
			if idx < 0 {
				return
			}
			at := i + idx + len("token=")
			if !strings.HasPrefix(out[at:], "[REDACTED]") {
				t.Fatalf("unredacted token value\n in: %q\nout: %q", in, out)
			}
			i = at
		}
	})
}
