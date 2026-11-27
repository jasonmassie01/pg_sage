package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/egress"
)

// The container healthcheck (ci-06): "sage-sidecar healthcheck" probes GET
// /health on the API listener the sidecar would bind, over HTTPS when the
// TLS pair is set. It is liveness only and sends no credential.

const defaultHealthcheckTimeout = 3 * time.Second

// healthcheckTarget is the URL the check probes. loopback allows skipping
// certificate verification: the probe never leaves the container then.
type healthcheckTarget struct {
	url      string
	loopback bool
}

// runHealthcheck returns the process exit code: 0 when /health answers 200,
// 1 otherwise, with stderr naming the URL and the status or error.
func runHealthcheck(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", "", "API address host:port (default: api.listen_addr "+
		"from SAGE_CONFIG_PATH or ./config.yaml, else "+config.DefaultAPIListenAddr+")")
	timeout := fs.Duration("timeout", defaultHealthcheckTimeout, "how long to wait for /health")
	if err := fs.Parse(args); err != nil {
		return healthcheckFailed(stderr, err)
	}
	if fs.NArg() > 0 {
		return healthcheckFailed(stderr, fmt.Errorf("unexpected argument %q", fs.Arg(0)))
	}
	if *timeout <= 0 {
		return healthcheckFailed(stderr, fmt.Errorf("--timeout must be positive, got %s",
			*timeout))
	}
	target, err := resolveHealthcheckTarget(*addr)
	if err != nil {
		return healthcheckFailed(stderr, err)
	}
	if err := probeHealth(healthcheckClient(target, *timeout), target, *timeout); err != nil {
		return healthcheckFailed(stderr, err)
	}
	_, _ = fmt.Fprintf(stdout, "healthcheck: %s answered 200\n", target.url)
	return 0
}

func healthcheckFailed(stderr io.Writer, err error) int {
	_, _ = fmt.Fprintf(stderr, "healthcheck: %v\n", err)
	return 1
}

// resolveHealthcheckTarget finds the API address as config.Load would,
// without the database settings: --addr, then api.listen_addr from
// SAGE_CONFIG_PATH or ./config.yaml, then the default.
func resolveHealthcheckTarget(addrFlag string) (healthcheckTarget, error) {
	https, err := healthcheckTLS()
	if err != nil {
		return healthcheckTarget{}, err
	}
	addr, source, err := healthcheckListenAddr(addrFlag)
	if err != nil {
		return healthcheckTarget{}, err
	}
	target, err := healthcheckTargetFor(addr, https)
	if err != nil {
		return healthcheckTarget{}, fmt.Errorf("%s: %w", source, err)
	}
	return target, nil
}

// healthcheckTLS mirrors config.TLSEnabled and its half-pair rejection.
func healthcheckTLS() (bool, error) {
	cert, key := os.Getenv("SAGE_TLS_CERT") != "", os.Getenv("SAGE_TLS_KEY") != ""
	if cert != key {
		return false, fmt.Errorf("SAGE_TLS_CERT and SAGE_TLS_KEY must be set together "+
			"(cert set: %t, key set: %t)", cert, key)
	}
	return cert, nil
}

func healthcheckListenAddr(addrFlag string) (addr, source string, err error) {
	if addrFlag != "" {
		return addrFlag, "--addr", nil
	}
	path := os.Getenv("SAGE_CONFIG_PATH")
	if path == "" {
		if _, statErr := os.Stat("config.yaml"); statErr == nil {
			path = "config.yaml"
		}
	}
	if path != "" {
		addr, err := config.APIListenAddrFromFile(path)
		if err != nil {
			return "", "", fmt.Errorf("read api.listen_addr from config %s: %w", path, err)
		}
		if addr != "" {
			return addr, "config " + path, nil
		}
	}
	return config.DefaultAPIListenAddr, "default", nil
}

// healthcheckTargetFor builds the /health URL for a listen address,
// dialling loopback for a wildcard host.
func healthcheckTargetFor(addr string, https bool) (healthcheckTarget, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return healthcheckTarget{}, fmt.Errorf("api.listen_addr %q is not host:port: %w",
			addr, err)
	}
	if n, convErr := strconv.Atoi(port); convErr != nil || n < 1 || n > 65535 {
		return healthcheckTarget{}, fmt.Errorf("api.listen_addr %q: port %q is not 1-65535",
			addr, port)
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	scheme := "http"
	if https {
		scheme = "https"
	}
	u := url.URL{Scheme: scheme, Host: net.JoinHostPort(host, port), Path: "/health"}
	return healthcheckTarget{url: u.String(), loopback: isLoopbackHost(host)}, nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// healthcheckClient ignores proxy settings and redirects; it skips
// certificate verification only for a loopback target, where the
// sidecar's own (often self-signed) certificate names another host.
func healthcheckClient(target healthcheckTarget, timeout time.Duration) *http.Client {
	transport := egress.Transport(timeout)
	transport.Proxy = nil
	transport.DisableKeepAlives = true
	transport.TLSClientConfig = &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: target.loopback,
	}
	client := egress.HTTPClient(0)
	client.Transport = transport
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return client
}

func probeHealth(client *http.Client, target healthcheckTarget, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.url, nil)
	if err != nil {
		return fmt.Errorf("build request for %s: %w", target.url, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		var netErr net.Error
		if errors.Is(err, context.DeadlineExceeded) ||
			(errors.As(err, &netErr) && netErr.Timeout()) {
			return fmt.Errorf("GET %s: no answer within %s", target.url, timeout)
		}
		// url.Error repeats the method and URL this message already names.
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return fmt.Errorf("GET %s: %w", target.url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: status %d, want 200", target.url, resp.StatusCode)
	}
	return nil
}
