package chatops

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"
)

// Signed callbacks (AI-SRE-SPEC R1.1): Slack's v0 HMAC-SHA256 signature
// over "v0:<timestamp>:<body>" with a timestamp tolerance, and Telegram's
// per-bot secret token header. Both fail closed: no secret configured
// means nothing verifies.

const slackSecret = "8f742231b10e8888abcd99yyyzzz85a5"

var slackNow = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func slackHeaders(secret string, ts time.Time, body []byte) http.Header {
	stamp := strconv.FormatInt(ts.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("v0:" + stamp + ":"))
	mac.Write(body)
	h := http.Header{}
	h.Set("X-Slack-Request-Timestamp", stamp)
	h.Set("X-Slack-Signature", "v0="+hex.EncodeToString(mac.Sum(nil)))
	return h
}

func TestVerifySlackAcceptsAValidSignature(t *testing.T) {
	body := []byte("payload=%7B%22type%22%3A%22block_actions%22%7D")
	h := slackHeaders(slackSecret, slackNow, body)
	if err := VerifySlack(slackSecret, h, body, slackNow, 5*time.Minute); err != nil {
		t.Fatalf("valid signature: %v", err)
	}
}

func TestVerifySlackTimestampTolerance(t *testing.T) {
	body := []byte("payload=x")
	tol := 5 * time.Minute
	for name, tc := range map[string]struct {
		signedAt time.Time
		want     error
	}{
		"at the past edge":     {slackNow.Add(-tol), nil},
		"past the past edge":   {slackNow.Add(-tol - time.Second), ErrStale},
		"at the future edge":   {slackNow.Add(tol), nil},
		"beyond future edge":   {slackNow.Add(tol + time.Second), ErrStale},
		"an hour old (replay)": {slackNow.Add(-time.Hour), ErrStale},
	} {
		h := slackHeaders(slackSecret, tc.signedAt, body)
		if err := VerifySlack(slackSecret, h, body, slackNow, tol); !errors.Is(err, tc.want) &&
			!(tc.want == nil && err == nil) {
			t.Errorf("%s: VerifySlack = %v, want %v", name, err, tc.want)
		}
	}
}

func TestVerifySlackRejectsForgeries(t *testing.T) {
	body := []byte("payload=x")
	good := slackHeaders(slackSecret, slackNow, body)
	for name, tc := range map[string]struct {
		secret string
		header func() http.Header
		body   []byte
	}{
		"other secret": {slackSecret, func() http.Header {
			return slackHeaders("not-the-secret", slackNow, body)
		}, body},
		"tampered body": {slackSecret, func() http.Header { return good }, []byte("payload=y")},
		"no signature": {slackSecret, func() http.Header {
			h := good.Clone()
			h.Del("X-Slack-Signature")
			return h
		}, body},
		"no timestamp": {slackSecret, func() http.Header {
			h := good.Clone()
			h.Del("X-Slack-Request-Timestamp")
			return h
		}, body},
		"bad timestamp": {slackSecret, func() http.Header {
			h := good.Clone()
			h.Set("X-Slack-Request-Timestamp", "noon")
			return h
		}, body},
		"other version": {slackSecret, func() http.Header {
			h := good.Clone()
			h.Set("X-Slack-Signature", "v1="+h.Get("X-Slack-Signature")[3:])
			return h
		}, body},
		"not hex": {slackSecret, func() http.Header {
			h := good.Clone()
			h.Set("X-Slack-Signature", "v0=zz")
			return h
		}, body},
		"no secret configured": {"", func() http.Header {
			return slackHeaders("", slackNow, body)
		}, body},
	} {
		err := VerifySlack(tc.secret, tc.header(), tc.body, slackNow, 5*time.Minute)
		if !errors.Is(err, ErrBadSignature) && !errors.Is(err, ErrStale) {
			t.Errorf("%s: VerifySlack = %v, want a refusal", name, err)
		}
		if err == nil {
			t.Errorf("%s: forged request verified", name)
		}
	}
}

func TestVerifyTelegramSecretToken(t *testing.T) {
	h := http.Header{}
	h.Set("X-Telegram-Bot-Api-Secret-Token", "s3cret-token_A")
	if err := VerifyTelegram("s3cret-token_A", h); err != nil {
		t.Fatalf("matching token: %v", err)
	}
	for name, tc := range map[string]struct {
		expected string
		got      string
	}{
		"other token":   {"s3cret-token_A", "s3cret-token_B"},
		"prefix only":   {"s3cret-token_A", "s3cret"},
		"missing":       {"s3cret-token_A", ""},
		"no secret set": {"", ""},
	} {
		h := http.Header{}
		if tc.got != "" {
			h.Set("X-Telegram-Bot-Api-Secret-Token", tc.got)
		}
		if err := VerifyTelegram(tc.expected, h); !errors.Is(err, ErrBadSignature) {
			t.Errorf("%s: VerifyTelegram = %v, want ErrBadSignature", name, err)
		}
	}
}
