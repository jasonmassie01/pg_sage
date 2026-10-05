package cloudtel

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

const maxResponseBytes = 8 << 20

// awsClient sends SigV4-signed requests to CloudWatch and Performance
// Insights. Credentials are retrieved per request from the provider's
// cache and never logged or included in errors.
type awsClient struct {
	creds  aws.CredentialsProvider
	http   *http.Client
	signer *v4.Signer
	region string
	now    func() time.Time
}

func (c *awsClient) credentials(ctx context.Context) (aws.Credentials, error) {
	creds, err := c.creds.Retrieve(ctx)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return aws.Credentials{}, ctxErr
		}
		return aws.Credentials{}, fmt.Errorf("%w: AWS default credential chain (environment, "+
			"shared config, web identity, instance/task role): %s", ErrNoCredentials,
			sanitize(err.Error()))
	}
	return creds, nil
}

// do signs and sends one request and returns the body of a 2xx response.
func (c *awsClient) do(ctx context.Context, service, endpoint, contentType string,
	headers map[string]string, body []byte) ([]byte, error) {
	creds, err := c.credentials(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%w: build %s request", ErrProvider, service)
	}
	req.Header.Set("Content-Type", contentType)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	sum := sha256.Sum256(body)
	if err := c.signer.SignHTTP(ctx, creds, req, hex.EncodeToString(sum[:]), service,
		c.region, c.now()); err != nil {
		return nil, fmt.Errorf("%w: sign %s request", ErrProvider, service)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: %s request failed; check network access to %s",
			ErrProvider, service, endpointHost(endpoint))
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil || len(raw) > maxResponseBytes {
		return nil, fmt.Errorf("%w: %s response unreadable or over %d bytes", ErrMalformed,
			service, maxResponseBytes)
	}
	if err := checkServerDate(resp.Header.Get("Date"), c.now()); err != nil {
		return nil, fmt.Errorf("%s: %w", service, err)
	}
	if resp.StatusCode/100 != 2 {
		return nil, classifyAWSError(service, resp.StatusCode, raw)
	}
	return raw, nil
}

// checkServerDate fails when the provider's clock and ours disagree by
// more than MaxClockSkew: datapoint freshness can then not be judged.
func checkServerDate(header string, now time.Time) error {
	if header == "" {
		return nil
	}
	at, err := http.ParseTime(header)
	if err != nil {
		return nil
	}
	if d := at.Sub(now); d > MaxClockSkew || d < -MaxClockSkew {
		return fmt.Errorf("%w: provider clock differs from the local clock by %s; fix NTP",
			ErrClockSkew, d.Round(time.Second))
	}
	return nil
}

type awsQueryError struct {
	Code    string `xml:"Error>Code"`
	Message string `xml:"Error>Message"`
}

// classifyAWSError maps Query (XML) and JSON protocol errors to the
// package's distinguishable errors.
func classifyAWSError(service string, status int, raw []byte) error {
	code, msg := "", ""
	var q awsQueryError
	if xml.Unmarshal(raw, &q) == nil && q.Code != "" {
		code, msg = q.Code, q.Message
	} else {
		var j struct {
			Type    string `json:"__type"`
			Message string `json:"message"`
			Msg     string `json:"Message"`
		}
		if json.Unmarshal(raw, &j) == nil {
			code, msg = j.Type, j.Message+j.Msg
			if i := strings.LastIndex(code, "#"); i >= 0 {
				code = code[i+1:]
			}
		}
	}
	return awsCodeError(service, status, code, msg)
}

func awsCodeError(service string, status int, code, msg string) error {
	detail := fmt.Sprintf("%s HTTP %d %s: %s", service, status, code, sanitize(msg))
	switch {
	case code == "RequestTimeTooSkewed" || code == "RequestExpired" ||
		strings.Contains(msg, "Signature expired") || strings.Contains(msg, "too skewed"):
		return fmt.Errorf("%w: %s", ErrClockSkew, detail)
	case strings.Contains(code, "Throttl") || code == "TooManyRequestsException" ||
		code == "RequestLimitExceeded" || status == http.StatusTooManyRequests:
		return fmt.Errorf("%w: %s", ErrThrottled, detail)
	case code == "AccessDenied" || code == "AccessDeniedException" ||
		code == "NotAuthorizedException" || code == "UnrecognizedClientException" ||
		code == "InvalidClientTokenId" || code == "ExpiredToken" ||
		code == "ExpiredTokenException" || code == "SignatureDoesNotMatch" ||
		code == "IncompleteSignature" || status == http.StatusUnauthorized ||
		status == http.StatusForbidden:
		return fmt.Errorf("%w: %s", ErrAuth, detail)
	case strings.Contains(code, "NotFound"):
		return fmt.Errorf("%w: %s", ErrIdentity, detail)
	}
	return fmt.Errorf("%w: %s", ErrProvider, detail)
}

// sanitize bounds provider text placed in errors and logs.
func sanitize(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	if r := []rune(s); len(r) > 300 {
		s = string(r[:300]) + "..."
	}
	return strings.TrimSpace(s)
}

func endpointHost(endpoint string) string {
	host := strings.TrimPrefix(strings.TrimPrefix(endpoint, "https://"), "http://")
	if i := strings.IndexByte(host, '/'); i >= 0 {
		host = host[:i]
	}
	return host
}
