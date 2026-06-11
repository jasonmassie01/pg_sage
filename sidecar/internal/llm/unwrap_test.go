package llm

import "testing"

func TestUnwrapText(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{"audit_note": "Lowered scale factor to 0.05."}`, "Lowered scale factor to 0.05."},
		{`{"narrative":"Plan flipped to seq scan due to stale stats."}`, "Plan flipped to seq scan due to stale stats."},
		{"Just plain prose, no JSON.", "Just plain prose, no JSON."},
		{"```json\n{\"note\":\"wrapped in fences\"}\n```", "wrapped in fences"},
		{`{"unknown_key":"still extracted"}`, "still extracted"},
		{"  spaced prose  ", "spaced prose"},
		{`{not valid json`, "{not valid json"},
	}
	for _, c := range cases {
		if got := UnwrapText(c.in); got != c.want {
			t.Errorf("UnwrapText(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
