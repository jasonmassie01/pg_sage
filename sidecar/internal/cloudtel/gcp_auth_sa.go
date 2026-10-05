package cloudtel

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// serviceAccountSource signs an RS256 JWT assertion with the key file's
// private key (standard library crypto) and exchanges it for a token.
func serviceAccountSource(f credentialFile, opts GCPAuthOptions) (*GCPToken, error) {
	key, err := parseRSAKey(f.PrivateKey)
	if err != nil || f.ClientEmail == "" {
		return nil, fmt.Errorf("%w: service account key file lacks a usable private key "+
			"or client_email", ErrMalformed)
	}
	tokenURI := f.TokenURI
	if tokenURI == "" {
		tokenURI = gcpTokenURL
	}
	return &GCPToken{kind: "service_account", project: f.ProjectID, now: opts.Now,
		fetch: func(ctx context.Context) (string, time.Duration, error) {
			assertion, err := signAssertion(key, f.PrivateKeyID, f.ClientEmail, tokenURI,
				opts.Now())
			if err != nil {
				return "", 0, err
			}
			form := url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
				"assertion": {assertion}}
			return postToken(ctx, opts.HTTPClient, tokenURI, form)
		}}, nil
}

func parseRSAKey(pemKey string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemKey))
	if block == nil {
		return nil, fmt.Errorf("no PEM block")
	}
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if rk, ok := k.(*rsa.PrivateKey); ok {
			return rk, nil
		}
		return nil, fmt.Errorf("not an RSA key")
	}
	return x509.ParsePKCS1PrivateKey(block.Bytes)
}

func signAssertion(key *rsa.PrivateKey, kid, email, aud string, now time.Time) (string,
	error) {
	header, err := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": kid})
	if err != nil {
		return "", fmt.Errorf("%w: encode JWT header", ErrProvider)
	}
	claims, err := json.Marshal(map[string]any{"iss": email, "scope": gcpScope, "aud": aud,
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix()})
	if err != nil {
		return "", fmt.Errorf("%w: encode JWT claims", ErrProvider)
	}
	enc := base64.RawURLEncoding
	unsigned := enc.EncodeToString(header) + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(unsigned))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", fmt.Errorf("%w: sign JWT assertion", ErrProvider)
	}
	return unsigned + "." + enc.EncodeToString(sig), nil
}

// metadataSource probes the metadata server (with a short timeout, so a
// host outside Google Cloud fails fast) and reads the project from it.
func metadataSource(ctx context.Context, host string, opts GCPAuthOptions) (*GCPToken, error) {
	base := "http://" + strings.TrimSuffix(host, "/")
	probeCtx, cancel := context.WithTimeout(ctx, metadataTimeout)
	defer cancel()
	project, err := metadataGet(probeCtx, opts.HTTPClient, base+"/computeMetadata/v1/project/"+
		"project-id")
	if err != nil {
		return nil, err
	}
	return &GCPToken{kind: "metadata", project: strings.TrimSpace(project), now: opts.Now,
		fetch: func(ctx context.Context) (string, time.Duration, error) {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+
				"/computeMetadata/v1/instance/service-accounts/default/token", nil)
			if err != nil {
				return "", 0, fmt.Errorf("%w: build metadata request", ErrProvider)
			}
			req.Header.Set("Metadata-Flavor", "Google")
			return doToken(ctx, opts.HTTPClient, req, "metadata server")
		}}, nil
}

func metadataGet(ctx context.Context, client *http.Client, u string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", errMetadataUnavailable
	}
	req.Header.Set("Metadata-Flavor", "Google")
	resp, err := client.Do(req)
	if err != nil {
		return "", errMetadataUnavailable
	}
	defer func() { _ = resp.Body.Close() }()
	flavor := resp.Header.Get("Metadata-Flavor")
	if resp.StatusCode != http.StatusOK || (flavor != "Google" && flavor != "") {
		return "", errMetadataUnavailable
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return "", errMetadataUnavailable
	}
	return string(raw), nil
}
