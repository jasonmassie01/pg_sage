// Package signed verifies machine-to-machine ingestion requests (signed
// change events and pushed SLI counters, AI-SRE-SPEC §9): an HMAC-SHA256
// over the timestamp, method, path and body, accepted only within a
// timestamp tolerance. Replays inside the tolerance are made harmless by
// the idempotent stores behind the endpoints (one row per source event,
// one sample per series and time). Verification keeps no state.
package signed

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"
)

// Request headers.
const (
	HeaderTimestamp = "X-Sage-Timestamp"
	HeaderSignature = "X-Sage-Signature"
)

// DefaultTolerance is the accepted clock difference when none is set.
const DefaultTolerance = 5 * time.Minute

// MinSecretBytes is the shortest accepted shared secret.
const MinSecretBytes = 32

const version = "v1"

// Verification errors; callers distinguish them with errors.Is.
var (
	ErrNotConfigured = errors.New("signed ingestion is not configured")
	ErrMissing       = errors.New("missing signature or timestamp")
	ErrMalformed     = errors.New("malformed signature or timestamp")
	ErrStale         = errors.New("timestamp outside the tolerance")
	ErrInvalid       = errors.New("signature does not verify")
)

// Request is the signed material of one HTTP request.
type Request struct {
	Method    string
	Path      string
	Timestamp string
	Signature string
	Body      []byte
}

// Sign returns the timestamp and signature headers for a request.
func Sign(secret []byte, method, path string, at time.Time, body []byte) (string, string) {
	ts := strconv.FormatInt(at.Unix(), 10)
	return ts, version + "=" + hex.EncodeToString(mac(secret, method, path, ts, body))
}

func mac(secret []byte, method, path, ts string, body []byte) []byte {
	h := hmac.New(sha256.New, secret)
	h.Write([]byte(version + ":" + ts + ":" + method + ":" + path + ":"))
	h.Write(body)
	return h.Sum(nil)
}

// Verify checks a request's signature and timestamp against now. It
// returns the signed time. A non-positive tolerance means the default.
func Verify(secret []byte, r Request, now time.Time, tolerance time.Duration) (time.Time, error) {
	if len(secret) == 0 {
		return time.Time{}, ErrNotConfigured
	}
	if r.Timestamp == "" || r.Signature == "" {
		return time.Time{}, ErrMissing
	}
	secs, err := strconv.ParseInt(r.Timestamp, 10, 64)
	if err != nil {
		return time.Time{}, ErrMalformed
	}
	got, ok := strings.CutPrefix(r.Signature, version+"=")
	if !ok || len(got) != sha256.Size*2 {
		return time.Time{}, ErrMalformed
	}
	sig, err := hex.DecodeString(got)
	if err != nil {
		return time.Time{}, ErrMalformed
	}
	if tolerance <= 0 {
		tolerance = DefaultTolerance
	}
	at := time.Unix(secs, 0).UTC()
	if skew := now.Sub(at); skew > tolerance || skew < -tolerance {
		return time.Time{}, ErrStale
	}
	if !hmac.Equal(sig, mac(secret, r.Method, r.Path, r.Timestamp, r.Body)) {
		return time.Time{}, ErrInvalid
	}
	return at, nil
}
