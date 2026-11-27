package store

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/notify"
)

// alerting-10: the channel list is flat JSON, so the dashboard reads
// last_status, last_error and last_sent_at beside the channel's own
// fields; a channel never delivered has a null last_sent_at.

func TestChannelListing_JSONIsFlat(t *testing.T) {
	at := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	l := ChannelListing{
		Channel: notify.Channel{
			ID: 7, Name: "ops", Type: "telegram", Enabled: true,
			Config: map[string]string{"chat_id": "-100"},
		},
		LastStatus: "error", LastError: "timeout", LastSentAt: &at,
	}
	raw, err := json.Marshal(l)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := map[string]any{
		"id": float64(7), "name": "ops", "type": "telegram", "enabled": true,
		"last_status": "error", "last_error": "timeout",
		"last_sent_at": "2030-01-02T03:04:05Z",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %#v, want %#v (json %s)", k, got[k], v, raw)
		}
	}
	cfg, ok := got["config"].(map[string]any)
	if !ok || cfg["chat_id"] != "-100" {
		t.Errorf("config = %#v, want the channel config", got["config"])
	}
}

func TestChannelListing_NeverDeliveredHasNullSentAt(t *testing.T) {
	raw, err := json.Marshal(ChannelListing{Channel: notify.Channel{ID: 1}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if v, ok := got["last_sent_at"]; !ok || v != nil {
		t.Errorf("last_sent_at = %#v (present %v), want null", v, ok)
	}
	if got["last_status"] != "" {
		t.Errorf("last_status = %#v, want empty", got["last_status"])
	}
}
