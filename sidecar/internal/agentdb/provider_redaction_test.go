package agentdb

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRedactProviderDetail(t *testing.T) {
	detail := map[string]any{
		"password": "secret",
		"nested": map[string]any{
			"Access_Key": "abc",
			"items": []any{
				map[string]any{"token": "tok"},
				"postgres://user:pass@example/db",
			},
		},
		"safe": "value",
	}

	got := RedactProviderDetail(detail)
	text := strings.ToLower(string(mustJSON(got)))
	for _, leaked := range []string{"secret", "abc", "tok\"", "pass@example"} {
		if strings.Contains(text, leaked) {
			t.Fatalf("redacted detail leaked %q: %s", leaked, text)
		}
	}
	if got["safe"] != "value" {
		t.Fatalf("safe value changed: %#v", got)
	}
}

func TestRedactProviderDetailCoversCloudCredentialShapes(t *testing.T) {
	detail := map[string]any{
		"api_key":       "AIza-do-not-leak",
		"Authorization": "Bearer live-token",
		"client_email":  "robot@example.iam.gserviceaccount.com",
		"raw_url":       "https://user:pass@example.com/path?token=secret-token",
		"aws_url":       "https://rds.amazonaws.com/?X-Amz-Signature=abc123",
		"safe":          "visible",
	}

	got := RedactProviderDetail(detail)
	text := strings.ToLower(string(mustJSON(got)))
	for _, leaked := range []string{
		"aiza", "live-token", "robot@example", "pass@example",
		"secret-token", "abc123",
	} {
		if strings.Contains(text, leaked) {
			t.Fatalf("redacted cloud detail leaked %q: %s", leaked, text)
		}
	}
	if got["safe"] != "visible" {
		t.Fatalf("safe value changed: %#v", got)
	}
}

func FuzzRedactProviderDetail(f *testing.F) {
	f.Add("password", "secret-value")
	f.Add("Token", "token-value")
	f.Fuzz(func(t *testing.T, key string, value string) {
		got := RedactProviderDetail(map[string]any{key: value})
		text := strings.ToLower(string(mustJSON(got)))
		if sensitiveKey(key) && strings.Contains(text, strings.ToLower(value)) {
			t.Fatalf("sensitive value leaked for key %q: %s", key, text)
		}
	})
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
