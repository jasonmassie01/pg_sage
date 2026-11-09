package egress

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestEgressControlRefusesFloorAddresses(t *testing.T) {
	cases := []string{
		"169.254.169.254:80", // cloud instance metadata service
		"169.254.0.1:443",
		"[fe80::1]:80",
		"[fd00:ec2::254]:80", // AWS IMDS over IPv6
		"224.0.0.1:80",
		"239.1.2.3:5000",
		"[ff02::1]:80",
		"[ff05::2]:80",
		"0.0.0.0:80",
		"[::]:80",
		"255.255.255.255:514",
		"[::ffff:169.254.169.254]:80", // IPv4-mapped metadata address
	}
	for _, address := range cases {
		err := Control("tcp", address, nil)
		if !errors.Is(err, ErrBlocked) {
			t.Errorf("Control(%s) = %v, want ErrBlocked", address, err)
		}
	}
}

func TestEgressControlAllowsPrivateAndLoopback(t *testing.T) {
	cases := []string{
		"127.0.0.1:11434", // Ollama on localhost
		"[::1]:9090",
		"10.1.2.3:9090", // Prometheus inside the VPC
		"172.16.0.5:25",
		"192.168.1.10:587",
		"100.64.1.1:443",
		"[fd12:3456::1]:443", // ULA other than the IMDS address
		"8.8.8.8:443",
		"169.253.255.255:80", // boundary below 169.254.0.0/16
		"169.255.0.0:80",     // boundary above 169.254.0.0/16
		"223.255.255.255:80", // boundary below 224.0.0.0/4
	}
	for _, address := range cases {
		if err := Control("tcp", address, nil); err != nil {
			t.Errorf("Control(%s) = %v, want nil", address, err)
		}
	}
}

func TestEgressControlInvalidAddress(t *testing.T) {
	for _, address := range []string{"", "no-port", "not-an-ip:80", "[fe80::1"} {
		if err := Control("tcp", address, nil); !errors.Is(err, ErrBlocked) {
			t.Errorf("Control(%q) = %v, want ErrBlocked", address, err)
		}
	}
}

func TestEgressCheckAddrZeroValue(t *testing.T) {
	if err := CheckAddr(netip.Addr{}); !errors.Is(err, ErrBlocked) {
		t.Fatalf("CheckAddr(zero) = %v, want ErrBlocked", err)
	}
}

func TestEgressDialerRefusesMetadataLiteral(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, address := range []string{
		"169.254.169.254:80", "[fe80::1]:80", "[fd00:ec2::254]:80", "224.0.0.1:80",
	} {
		conn, err := Dialer(time.Second).DialContext(ctx, "tcp", address)
		if conn != nil {
			_ = conn.Close()
		}
		if !errors.Is(err, ErrBlocked) {
			t.Errorf("dial %s = %v, want ErrBlocked", address, err)
		}
	}
}

func TestEgressDialerReachesLoopbackListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		conn, acceptErr := ln.Accept()
		if acceptErr == nil {
			_, _ = conn.Write([]byte("hi"))
			_ = conn.Close()
		}
	}()
	conn, err := Dialer(2*time.Second).Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial loopback listener: %v", err)
	}
	defer func() { _ = conn.Close() }()
	got, err := io.ReadAll(conn)
	if err != nil || string(got) != "hi" {
		t.Fatalf("read = %q, %v; want hi", got, err)
	}
}

func TestEgressDialerRefusesHostnameResolvingToLinkLocal(t *testing.T) {
	d := Dialer(2 * time.Second)
	d.Resolver = fakeResolver(t, netip.MustParseAddr("169.254.169.254"))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := d.DialContext(ctx, "tcp", "imds.attacker.test:80")
	if conn != nil {
		_ = conn.Close()
	}
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("dial name resolving to 169.254.169.254 = %v, want ErrBlocked", err)
	}
}

func TestEgressHTTPClientDeliversToLoopback(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got = string(body)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	resp, err := HTTPClient(5*time.Second).Post(srv.URL, "text/plain", strings.NewReader("ok"))
	if err != nil {
		t.Fatalf("post to loopback: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted || got != "ok" {
		t.Fatalf("status %d body %q, want 202 ok", resp.StatusCode, got)
	}
}

func TestEgressHTTPClientRefusesMetadataURL(t *testing.T) {
	client := HTTPClient(2 * time.Second)
	for _, raw := range []string{
		"http://169.254.169.254/latest/meta-data/",
		"http://[fd00:ec2::254]/latest/meta-data/",
		"http://metadata.google.internal/computeMetadata/v1/",
		"http://METADATA.google.internal./computeMetadata/v1/",
	} {
		resp, err := client.Get(raw)
		if resp != nil {
			_ = resp.Body.Close()
		}
		if !errors.Is(err, ErrBlocked) {
			t.Errorf("GET %s = %v, want ErrBlocked", raw, err)
		}
	}
}

func TestEgressHTTPClientRefusesRedirectToMetadata(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	defer srv.Close()
	resp, err := HTTPClient(2 * time.Second).Get(srv.URL)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("redirect to metadata = %v, want ErrBlocked", err)
	}
}

func TestEgressHTTPClientTimeout(t *testing.T) {
	if got := HTTPClient(7 * time.Second).Timeout; got != 7*time.Second {
		t.Fatalf("Timeout = %v, want 7s", got)
	}
	if got := HTTPClient(0).Timeout; got != 0 {
		t.Fatalf("Timeout(0) = %v, want 0 (caller uses context deadlines)", got)
	}
	if got := Dialer(0).Timeout; got != defaultDialTimeout {
		t.Fatalf("Dialer(0).Timeout = %v, want %v", got, defaultDialTimeout)
	}
}

func TestEgressHTTPClientConcurrentUse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	client := HTTPClient(5 * time.Second)
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			target := srv.URL
			if i%2 == 1 {
				target = "http://169.254.169.254/"
			}
			resp, err := client.Get(target)
			if resp != nil {
				_ = resp.Body.Close()
			}
			if i%2 == 0 && (err != nil || resp.StatusCode != http.StatusNoContent) {
				errs <- err
			}
			if i%2 == 1 && !errors.Is(err, ErrBlocked) {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent request outcome wrong: %v", err)
	}
}
