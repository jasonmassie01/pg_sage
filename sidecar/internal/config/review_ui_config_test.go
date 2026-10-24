package config

import (
	"strings"
	"testing"
)

// change_review.ui_base_url is the address people open pg_sage's UI at:
// the PR comment and the needs-DBA notification link reviews through it.
// Empty (the default) links nothing; anything set must be an absolute
// http(s) URL without query or fragment, and the error names the key.
// Stateless: no concurrency test.
func TestChangeReviewUIBaseURL(t *testing.T) {
	if got := DefaultChangeReviewConfig().UIBaseURL; got != "" {
		t.Fatalf("default ui_base_url %q, want empty", got)
	}
	for _, ok := range []string{"", "https://sage.example.com",
		"http://10.0.0.5:8080", "https://example.com/pg-sage/"} {
		c := DefaultChangeReviewConfig()
		c.UIBaseURL = ok
		if err := c.Validate(); err != nil {
			t.Errorf("ui_base_url %q: %v", ok, err)
		}
	}
	for _, bad := range []string{"sage.example.com", "ftp://sage.example.com",
		"https://sage.example.com/?x=1", "https://sage.example.com/#/reviews",
		"https://user:pw@sage.example.com", "https://", "/#/reviews", " https://x.y"} {
		c := DefaultChangeReviewConfig()
		c.UIBaseURL = bad
		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), "change_review.ui_base_url") {
			t.Errorf("ui_base_url %q: %v, want an error naming the key", bad, err)
		}
	}
}
