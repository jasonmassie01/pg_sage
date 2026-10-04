package api

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/url"
	"sync"
	"testing"
	"time"
)

// captureProxy forwards TCP connections to target and records every byte
// the client sends, which is what the server reads.
type captureProxy struct {
	ln     net.Listener
	target string
	mu     sync.Mutex
	sent   bytes.Buffer
	wg     sync.WaitGroup
}

func startCaptureProxy(t *testing.T, target string) *captureProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	p := &captureProxy{ln: ln, target: target}
	p.wg.Add(1)
	go p.serve()
	t.Cleanup(func() {
		_ = ln.Close()
		p.wg.Wait()
	})
	return p
}

func (p *captureProxy) serve() {
	defer p.wg.Done()
	for {
		client, err := p.ln.Accept()
		if err != nil {
			return // listener closed
		}
		p.wg.Add(1)
		go p.forward(client)
	}
}

func (p *captureProxy) forward(client net.Conn) {
	defer p.wg.Done()
	defer func() { _ = client.Close() }()
	server, err := net.DialTimeout("tcp", p.target, 10*time.Second)
	if err != nil {
		return
	}
	defer func() { _ = server.Close() }()
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(client, server)
		_ = client.Close()
		close(done)
	}()
	_, _ = io.Copy(server, io.TeeReader(client, p))
	_ = server.Close()
	<-done
}

// Write records client bytes (io.TeeReader's writer).
func (p *captureProxy) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sent.Write(b)
}

func (p *captureProxy) clientBytes() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]byte(nil), p.sent.Bytes()...)
}

// The API's connection test is pg_sage's own work on the database being
// tested: its session is named pg_sage and its statements carry the
// pg_sage tag, like every pooled pg_sage connection, so the database's
// pg_stat_statements and pg_stat_activity show it as pg_sage's.
func TestTestFromConnString_IdentifiesItselfAsPgSage(t *testing.T) {
	_, _ = phase2RequireDB(t)
	u, err := url.Parse(phase2DSN())
	if err != nil || u.Host == "" {
		t.Fatalf("test DSN is not a URL: %v", err)
	}
	proxy := startCaptureProxy(t, u.Host)
	q := u.Query()
	q.Set("sslmode", "disable") // the capture must read clear text
	q.Set("application_name", "someone_else")
	u.Host, u.RawQuery = proxy.ln.Addr().String(), q.Encode()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result := testFromConnString(ctx, u.String())
	if result.Status != "ok" {
		t.Fatalf("connection test through the proxy: status %q, error %q",
			result.Status, result.Error)
	}
	sent := proxy.clientBytes()
	for _, want := range []string{
		"application_name\x00pg_sage\x00",
		"SELECT /* pg_sage */ version()",
		"SELECT /* pg_sage */ 1 FROM pg_extension",
	} {
		if !bytes.Contains(sent, []byte(want)) {
			t.Errorf("server never received %q", want)
		}
	}
	for _, unwanted := range []string{"someone_else", "SELECT version()"} {
		if bytes.Contains(sent, []byte(unwanted)) {
			t.Errorf("server received %q: the session is not identified as pg_sage", unwanted)
		}
	}
}
