// Package chatops verifies and parses interactive approval callbacks
// from Slack and Telegram and maps the chat user who pressed a button to
// a pg_sage account. It decides nothing itself: the approval API applies
// the decision with the mapped user's attribution.
package chatops

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Providers of interactive callbacks.
const (
	ProviderSlack    = "slack"
	ProviderTelegram = "telegram"
)

var (
	// ErrBadSignature means the request is not signed by the configured
	// secret (or no secret is configured).
	ErrBadSignature = errors.New("chatops: bad signature")
	// ErrStale means a signed request is outside the timestamp tolerance.
	ErrStale = errors.New("chatops: stale request")
)

// VerifySlack checks Slack's v0 request signature: HMAC-SHA256 with the
// signing secret over "v0:<timestamp>:<body>", and a timestamp within
// tolerance of now in either direction.
func VerifySlack(secret string, h http.Header, body []byte, now time.Time,
	tolerance time.Duration) error {
	if secret == "" {
		return fmt.Errorf("%w: no signing secret configured", ErrBadSignature)
	}
	stamp := h.Get("X-Slack-Request-Timestamp")
	sig := h.Get("X-Slack-Signature")
	if stamp == "" || sig == "" {
		return fmt.Errorf("%w: missing signature headers", ErrBadSignature)
	}
	secs, err := strconv.ParseInt(stamp, 10, 64)
	if err != nil {
		return fmt.Errorf("%w: timestamp is not a number", ErrBadSignature)
	}
	got, ok := strings.CutPrefix(sig, "v0=")
	if !ok {
		return fmt.Errorf("%w: unsupported signature version", ErrBadSignature)
	}
	raw, err := hex.DecodeString(got)
	if err != nil {
		return fmt.Errorf("%w: signature is not hex", ErrBadSignature)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("v0:" + stamp + ":"))
	mac.Write(body)
	if !hmac.Equal(raw, mac.Sum(nil)) {
		return ErrBadSignature
	}
	skew := now.Sub(time.Unix(secs, 0))
	if skew > tolerance || skew < -tolerance {
		return fmt.Errorf("%w: signed %s from now", ErrStale, skew.Round(time.Second))
	}
	return nil
}

// VerifyTelegram checks the per-bot secret token Telegram sends with
// every webhook delivery (set with setWebhook's secret_token).
func VerifyTelegram(expected string, h http.Header) error {
	if expected == "" {
		return fmt.Errorf("%w: no webhook secret configured", ErrBadSignature)
	}
	got := h.Get("X-Telegram-Bot-Api-Secret-Token")
	if subtle.ConstantTimeCompare([]byte(got), []byte(expected)) != 1 {
		return ErrBadSignature
	}
	return nil
}
