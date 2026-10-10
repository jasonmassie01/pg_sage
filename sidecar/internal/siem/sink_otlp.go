package siem

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type otlpSink struct {
	name, url, token string
	client           *http.Client
}

// NewOTLPSink sends events as OTLP/HTTP JSON log records: a bare endpoint
// gets /v1/logs appended; a full path is used as given.
func NewOTLPSink(name, endpoint, token string, timeout time.Duration) Sink {
	if u, err := url.Parse(endpoint); err == nil && (u.Path == "" || u.Path == "/") {
		endpoint = strings.TrimRight(endpoint, "/") + "/v1/logs"
	}
	return &otlpSink{name: name, url: endpoint, token: token,
		client: &http.Client{Timeout: timeout}}
}

func (s *otlpSink) Name() string { return s.name }

func (s *otlpSink) Send(ctx context.Context, events []Event) error {
	if len(events) == 0 {
		return nil
	}
	records := make([]map[string]any, 0, len(events))
	for _, e := range events {
		rec, err := logRecord(e)
		if err != nil {
			return fmt.Errorf("siem sink %s: encode: %w", s.name, err)
		}
		records = append(records, rec)
	}
	body, err := json.Marshal(map[string]any{"resourceLogs": []any{map[string]any{
		"resource": map[string]any{"attributes": []any{
			strAttr("service.name", "pg_sage")}},
		"scopeLogs": []any{map[string]any{"scope": map[string]any{"name": "pg_sage.audit"},
			"logRecords": records}},
	}}})
	if err != nil {
		return fmt.Errorf("siem sink %s: encode: %w", s.name, err)
	}
	reply, err := post(ctx, s.client, s.name, s.url, s.token, body)
	if err != nil {
		return err
	}
	return partialRejection(s.name, reply)
}

// logRecord is one OTLP log record carrying an OCSF event as its body.
func logRecord(e Event) (map[string]any, error) {
	body, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}
	ms, _ := e["time"].(int64)
	sev, _ := e["severity_id"].(int)
	class, _ := e["class_uid"].(int)
	ext, _ := e["unmapped"].(map[string]any)["pg_sage"].(map[string]any)
	chain, _ := ext["chain"].(string)
	seq, _ := ext["seq"].(int64)
	num, text := otlpSeverity(sev)
	return map[string]any{
		"timeUnixNano":         strconv.FormatInt(ms*1_000_000, 10),
		"observedTimeUnixNano": strconv.FormatInt(time.Now().UnixNano(), 10),
		"severityNumber":       num, "severityText": text,
		"body": map[string]any{"stringValue": string(body)},
		"attributes": []any{intAttr("ocsf.class_uid", int64(class)),
			strAttr("pg_sage.chain", chain), intAttr("pg_sage.seq", seq)},
	}, nil
}

func strAttr(k, v string) map[string]any {
	return map[string]any{"key": k, "value": map[string]any{"stringValue": v}}
}

// intAttr encodes an int64 as OTLP JSON does: a decimal string.
func intAttr(k string, v int64) map[string]any {
	return map[string]any{"key": k,
		"value": map[string]any{"intValue": strconv.FormatInt(v, 10)}}
}

func otlpSeverity(ocsf int) (int, string) {
	switch ocsf {
	case SeverityMedium:
		return 13, "WARN"
	case SeverityHigh:
		return 17, "ERROR"
	}
	return 9, "INFO"
}

// partialRejection turns an OTLP partial success that rejected records
// into an error, so the batch is retried rather than silently lost.
func partialRejection(name string, reply []byte) error {
	var r struct {
		Partial *struct {
			Rejected json.RawMessage `json:"rejectedLogRecords"`
			Message  string          `json:"errorMessage"`
		} `json:"partialSuccess"`
	}
	if len(reply) == 0 || json.Unmarshal(reply, &r) != nil || r.Partial == nil {
		return nil
	}
	n := strings.Trim(string(r.Partial.Rejected), `"`)
	if n == "" || n == "0" {
		return nil
	}
	return fmt.Errorf("siem sink %s: receiver rejected %s log records: %s", name, n,
		r.Partial.Message)
}
