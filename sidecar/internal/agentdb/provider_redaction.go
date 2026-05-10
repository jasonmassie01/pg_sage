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
	"private-key",
	"privatekey",
	"connection_string",
	"dsn",
	"access_key",
	"access-key",
	"accesskey",
	"api_key",
	"apikey",
	"authorization",
	"bearer",
	"client_email",
	"client-email",
	"private_key_id",
	"private-key-id",
	"x-amz",
	"signature",
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
	if redactedURL, ok := redactSensitiveURL(value); ok {
		return redactedURL
	}
	for _, part := range sensitiveKeyParts {
		if strings.Contains(lower, part+"=") || strings.Contains(lower, part+":") {
			return redactedValue
		}
	}
	return value
}

func redactSensitiveURL(value string) (string, bool) {
	u, err := url.Parse(value)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", false
	}
	if u.User == nil && !urlHasSensitiveQuery(u) {
		return value, false
	}
	user := u.User.Username()
	if user == "" {
		u.User = nil
	} else {
		u.User = url.UserPassword(user, redactedValue)
	}
	if urlHasSensitiveQuery(u) {
		q := u.Query()
		for key := range q {
			if sensitiveKey(key) {
				q.Set(key, redactedValue)
			}
		}
		u.RawQuery = q.Encode()
	}
	return fmt.Sprint(u), true
}

func urlHasSensitiveQuery(u *url.URL) bool {
	for key := range u.Query() {
		if sensitiveKey(key) {
			return true
		}
	}
	return false
}
