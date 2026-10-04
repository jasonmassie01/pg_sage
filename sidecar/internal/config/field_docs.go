package config

import (
	"reflect"
	"strings"
)

// FieldDoc explains one configuration key: its documentation and
// warning tags, whether it is secret, its lifecycle, and its current
// value when that value can be shown. Ask Sage's explain_config tool
// serves it to the model, so a value that is, or may carry, a credential
// is never included.
type FieldDoc struct {
	Path      string `json:"path"`
	Doc       string `json:"doc"`
	Warning   string `json:"warning,omitempty"`
	Secret    bool   `json:"secret"`
	Lifecycle string `json:"lifecycle"`
	HasValue  bool   `json:"has_value"`
	Value     any    `json:"value,omitempty"`
}

// credentialWords mark keys whose values may carry a credential even
// when they are not tagged secret (a DSN, an endpoint with user info).
// They are matched against the words of the key's last segment, so
// "daily_tokens_per_user" (tokens) is shown and "api_key" is not; a
// "token" word hides the key only at the end ("bot_token", not
// "token_budget_daily").
var credentialWords = map[string]bool{"password": true, "secret": true, "key": true,
	"url": true, "dsn": true, "endpoint": true, "credential": true, "credentials": true,
	"db": true}

// DescribeField explains the leaf key path (a YAML path such as
// "ask.retention_days") of cfg, or of the defaults when cfg is nil.
// Unknown and non-leaf paths report false.
func DescribeField(cfg *Config, path string) (FieldDoc, bool) {
	initializeLifecycleRegistry()
	entry, ok := lifecycleByPath[path]
	if !ok {
		return FieldDoc{}, false
	}
	if cfg == nil {
		cfg = DefaultConfig()
	}
	field := reflect.TypeOf(Config{}).FieldByIndex(entry.index)
	doc := FieldDoc{Path: path, Doc: field.Tag.Get("doc"), Warning: field.Tag.Get("warning"),
		Secret: field.Tag.Get("secret") == "true", Lifecycle: string(entry.Lifecycle)}
	if doc.Secret || mayCarryCredential(path) {
		return doc, true
	}
	value := reflect.ValueOf(cfg).Elem().FieldByIndex(entry.index)
	if showable(value.Type()) {
		doc.HasValue, doc.Value = true, value.Interface()
	}
	return doc, true
}

func mayCarryCredential(path string) bool {
	last := strings.ToLower(path[strings.LastIndexByte(path, '.')+1:])
	if last == "token" || strings.HasSuffix(last, "_token") {
		return true
	}
	for _, w := range strings.Split(last, "_") {
		if credentialWords[w] {
			return true
		}
	}
	return false
}

// showable reports scalar types and slices of scalars; structs, maps and
// slices of structs (database lists with passwords) are never shown.
func showable(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Bool, reflect.String, reflect.Int, reflect.Int8, reflect.Int16,
		reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16,
		reflect.Uint32, reflect.Uint64, reflect.Float32, reflect.Float64:
		return true
	case reflect.Slice:
		return showable(t.Elem()) && t.Elem().Kind() != reflect.Slice
	}
	return false
}
