package specialist

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// The generic signed webhook: a WebhookRequest body, X-Sage-Timestamp
// (unix seconds) and X-Sage-Signature: sha256=<hex HMAC of
// "timestamp.body">. The timestamp bounds replays to the tolerance; within
// it a repeat is idempotent (the same trigger or idempotency key coalesces).
// Results of webhook-opened investigations are posted, signed the same
// way, to the operator-configured result URL.

// WebhookAdapter verifies generic webhooks.
type WebhookAdapter struct {
	Secret    string
	Tolerance time.Duration
}

// SignWebhook is the X-Sage-Signature of body at timestamp.
func SignWebhook(secret, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp + "."))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// VerifyWebhookSignature checks the signature and that the timestamp is
// within tolerance of now (inclusive).
func VerifyWebhookSignature(secret string, body []byte, timestamp, signature string,
	now time.Time, tolerance time.Duration) error {
	if secret == "" || timestamp == "" || signature == "" {
		return ErrSignature
	}
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return ErrSignature
	}
	skew := now.Sub(time.Unix(ts, 0))
	if skew > tolerance || skew < -tolerance {
		return ErrSignature
	}
	got, ok := strings.CutPrefix(signature, "sha256=")
	want := strings.TrimPrefix(SignWebhook(secret, timestamp, body), "sha256=")
	if !ok || !hmac.Equal([]byte(got), []byte(want)) {
		return ErrSignature
	}
	return nil
}

func (h *handler) webhook(w http.ResponseWriter, r *http.Request, id Identity) {
	a := h.opts.Webhook
	if a == nil {
		writeError(w, ErrDisabled)
		return
	}
	body, err := readAdapterBody(r)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := VerifyWebhookSignature(a.Secret, body, r.Header.Get("X-Sage-Timestamp"),
		r.Header.Get("X-Sage-Signature"), h.opts.Now(), a.Tolerance); err != nil {
		writeError(w, err)
		return
	}
	var req WebhookRequest
	if err := decodeStrict(bytes.NewReader(body), maxAdapterBody, &req); err != nil {
		writeError(w, err)
		return
	}
	h.respondOpen(w, r.Context(), id, req.Database, req.Request)
}
