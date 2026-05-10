package agentdb

import (
	"fmt"
	"net/url"
	"strings"
)

const redactedValue = "[redacted]"

var sensitiveKeyParts = []string{
	"password",
	"token",
	"secret",
	"credential",
	"private_key",
	"connection_string",
	"dsn",
	"access_key",
	"session",
}

func RedactProviderDetail(detail map[string]any) map[string]any {
	if detail == nil {
		return map[string]any{}
	}
	redacted, ok := redactValue(detail).(map[string]any)
	if !ok {
		return map[string]any{}
	}
	return redacted
}

func redactValue(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			if sensitiveKey(key) {
				out[key] = redactedValue
				continue
			}
			out[key] = redactValue(item)
		}
		return out
	case []any:
		out := make([]any, 0, len(v))
		for _, item := range v {
			out = append(out, redactValue(item))
		}
		return out
	case []string:
		out := make([]any, 0, len(v))
		for _, item := range v {
			out = append(out, redactString(item))
		}
		return out
	case string:
		return redactString(v)
	default:
		return v
	}
}

func sensitiveKey(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	for _, part := range sensitiveKeyParts {
		if strings.Contains(key, part) {
			return true
		}
	}
	return false
}

func redactString(value string) string {
	lower := strings.ToLower(value)
	if strings.Contains(lower, "postgres://") ||
		strings.Contains(lower, "postgresql://") {
		return redactURLPassword(value)
	}
	for _, part := range sensitiveKeyParts {
		if strings.Contains(lower, part+"=") || strings.Contains(lower, part+":") {
			return redactedValue
		}
	}
	return value
}

func redactURLPassword(value string) string {
	u, err := url.Parse(value)
	if err != nil || u.User == nil {
		return redactedValue
	}
	user := u.User.Username()
	if user == "" {
		u.User = nil
	} else {
		u.User = url.UserPassword(user, redactedValue)
	}
	return fmt.Sprint(u)
}
