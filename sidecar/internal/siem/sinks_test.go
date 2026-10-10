package siem

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func sampleEvents(n int) []Event {
	out := make([]Event, 0, n)
	for i := 1; i <= n; i++ {
		e := Map(Record{Source: "orders", Chain: "action_log",
			Link: link("action_log", int64(i), "I"),
			Row:  map[string]any{"sql_executed": "CREATE INDEX i" + strconv.Itoa(i)}})
		out = append(out, e)
	}
	return out
}

// The HTTP sink posts one JSON array per batch with the bearer token.
func TestHTTPSinkPostsBatch(t *testing.T) {
	var got []map[string]any
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		if r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "type", http.StatusUnsupportedMediaType)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
	}))
	defer srv.Close()
	s := NewHTTPSink("soc", srv.URL, "tok123", time.Second)
	if err := s.Send(context.Background(), sampleEvents(3)); err != nil {
		t.Fatalf("send: %v", err)
	}
	if len(got) != 3 || auth != "Bearer tok123" || got[2]["class_uid"] != float64(6005) {
		t.Fatalf("received %d events, auth %q, first %v", len(got), auth, got)
	}
	if s.Name() != "soc" {
		t.Fatalf("name = %q", s.Name())
	}
}

// Any non-2xx is an error that names the status but never the token.
func TestHTTPSinkErrors(t *testing.T) {
	for _, status := range []int{400, 401, 429, 500, 503} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,
			_ *http.Request) {
			w.WriteHeader(status)
		}))
		err := NewHTTPSink("soc", srv.URL, "secret-token", time.Second).
			Send(context.Background(), sampleEvents(1))
		srv.Close()
		if err == nil || !strings.Contains(err.Error(), strconv.Itoa(status)) ||
			strings.Contains(err.Error(), "secret-token") {
			t.Fatalf("status %d: err = %v", status, err)
		}
	}
	err := NewHTTPSink("soc", "http://127.0.0.1:1", "", time.Second).
		Send(context.Background(), sampleEvents(1))
	if err == nil {
		t.Fatalf("unreachable receiver: no error")
	}
	if err := NewHTTPSink("soc", "http://127.0.0.1:1", "", time.Second).
		Send(context.Background(), nil); err != nil {
		t.Fatalf("empty batch must be a no-op: %v", err)
	}
}

// A hung receiver cannot hold the exporter past the sink's timeout.
func TestHTTPSinkTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		<-release
	}))
	defer srv.Close()
	defer close(release)
	start := time.Now()
	err := NewHTTPSink("soc", srv.URL, "", 200*time.Millisecond).
		Send(context.Background(), sampleEvents(1))
	if err == nil || time.Since(start) > 3*time.Second {
		t.Fatalf("hung receiver: err=%v after %v", err, time.Since(start))
	}
}

// OTLP/HTTP JSON: logs go to /v1/logs, one log record per event, the OCSF
// event as the body and its class as an attribute; int64s are strings.
func TestOTLPSinkBody(t *testing.T) {
	var body map[string]any
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	if err := NewOTLPSink("otel", srv.URL, "", time.Second).Send(context.Background(),
		sampleEvents(2)); err != nil {
		t.Fatalf("send: %v", err)
	}
	if path != "/v1/logs" {
		t.Fatalf("path = %q", path)
	}
	rl := body["resourceLogs"].([]any)[0].(map[string]any)
	records := rl["scopeLogs"].([]any)[0].(map[string]any)["logRecords"].([]any)
	if len(records) != 2 {
		t.Fatalf("records = %d", len(records))
	}
	rec := records[0].(map[string]any)
	if _, ok := rec["timeUnixNano"].(string); !ok {
		t.Fatalf("timeUnixNano not a string: %#v", rec["timeUnixNano"])
	}
	var inner map[string]any
	if err := json.Unmarshal([]byte(rec["body"].(map[string]any)["stringValue"].(string)),
		&inner); err != nil || inner["class_uid"] != float64(6005) {
		t.Fatalf("body = %v (%v)", inner, err)
	}
	attrs, _ := json.Marshal(rec["attributes"])
	if !strings.Contains(string(attrs), `"ocsf.class_uid"`) ||
		!strings.Contains(string(attrs), `"intValue":"6005"`) {
		t.Fatalf("attributes = %s", attrs)
	}
	if !strings.Contains(mustJSON(t, rl["resource"]), `"service.name"`) {
		t.Fatalf("resource lacks service.name")
	}
}

// A partial-success response that rejected records is an error.
func TestOTLPPartialRejection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"partialSuccess":{"rejectedLogRecords":"1",` +
			`"errorMessage":"bad"}}`))
	}))
	defer srv.Close()
	err := NewOTLPSink("otel", srv.URL+"/v1/logs", "", time.Second).
		Send(context.Background(), sampleEvents(2))
	if err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("partial rejection: err = %v", err)
	}
}

var syslogHeader = regexp.MustCompile(`^<(\d+)>1 \S+ \S+ pg_sage \S+ (\S+) - \{`)

// Syslog over TCP: RFC 5424 messages, octet-counting framing (RFC 6587).
func TestSyslogTCPFraming(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	msgs := make(chan string, 10)
	go readFramed(ln, msgs)
	s := NewSyslogSink("rsys", "tcp", ln.Addr().String(), time.Second, nil)
	defer s.(io.Closer).Close()
	if err := s.Send(context.Background(), sampleEvents(3)); err != nil {
		t.Fatalf("send: %v", err)
	}
	for i := 0; i < 3; i++ {
		m := waitMsg(t, msgs)
		parts := syslogHeader.FindStringSubmatch(m)
		if parts == nil {
			t.Fatalf("not RFC 5424: %q", m)
		}
		// facility 13 (log audit) * 8 + severity 6 (informational)
		if parts[1] != "110" || parts[2] != "6005" {
			t.Fatalf("pri %s msgid %s", parts[1], parts[2])
		}
	}
}

// Syslog over UDP: one datagram per event.
func TestSyslogUDP(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer pc.Close()
	s := NewSyslogSink("rsys", "udp", pc.LocalAddr().String(), time.Second, nil)
	defer s.(io.Closer).Close()
	if err := s.Send(context.Background(), sampleEvents(2)); err != nil {
		t.Fatalf("send: %v", err)
	}
	buf := make([]byte, 70000)
	for i := 0; i < 2; i++ {
		_ = pc.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, _, err := pc.ReadFrom(buf)
		if err != nil || !syslogHeader.Match(buf[:n]) {
			t.Fatalf("datagram %d: %q (%v)", i, buf[:n], err)
		}
	}
}

// Syslog over TLS (RFC 5425) verifies the receiver's certificate.
func TestSyslogTLS(t *testing.T) {
	ts := httptest.NewTLSServer(http.NotFoundHandler())
	cert := ts.TLS.Certificates
	roots := x509.NewCertPool()
	roots.AddCert(ts.Certificate())
	ts.Close()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: cert,
		MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	msgs := make(chan string, 10)
	go readFramed(ln, msgs)
	good := NewSyslogSink("rsys", "tls", ln.Addr().String(), 2*time.Second,
		&tls.Config{RootCAs: roots, ServerName: "example.com", MinVersion: tls.VersionTLS12})
	defer good.(io.Closer).Close()
	if err := good.Send(context.Background(), sampleEvents(1)); err != nil {
		t.Fatalf("tls send: %v", err)
	}
	if m := waitMsg(t, msgs); !syslogHeader.MatchString(m) {
		t.Fatalf("tls message %q", m)
	}
	untrusted := NewSyslogSink("rsys", "tls", ln.Addr().String(), 2*time.Second, nil)
	defer untrusted.(io.Closer).Close()
	if err := untrusted.Send(context.Background(), sampleEvents(1)); err == nil {
		t.Fatalf("an untrusted certificate was accepted")
	}
}

// A receiver that drops the connection is reconnected on the next send.
func TestSyslogReconnects(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	var mu sync.Mutex
	var conns int
	msgs := make(chan string, 10)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns++
			first := conns == 1
			mu.Unlock()
			if first {
				_ = c.Close()
				continue
			}
			go readConn(c, msgs)
		}
	}()
	s := NewSyslogSink("rsys", "tcp", ln.Addr().String(), time.Second, nil)
	defer s.(io.Closer).Close()
	var sendErr error
	for i := 0; i < 5; i++ {
		if sendErr = s.Send(context.Background(), sampleEvents(1)); sendErr == nil {
			select {
			case <-msgs:
				return
			case <-time.After(300 * time.Millisecond):
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("no delivery after reconnects (last error %v)", sendErr)
}

func readFramed(ln net.Listener, out chan<- string) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go readConn(c, out)
	}
}

// readConn parses octet-counted frames: "<len> <msg>".
func readConn(c net.Conn, out chan<- string) {
	defer c.Close()
	r := bufio.NewReader(c)
	for {
		lenStr, err := r.ReadString(' ')
		if err != nil {
			return
		}
		n, err := strconv.Atoi(strings.TrimSpace(lenStr))
		if err != nil {
			out <- "BAD FRAME " + lenStr
			return
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(r, buf); err != nil {
			return
		}
		out <- string(buf)
	}
}

func waitMsg(t *testing.T, ch <-chan string) string {
	t.Helper()
	select {
	case m := <-ch:
		return m
	case <-time.After(3 * time.Second):
		t.Fatalf("no syslog message received")
	}
	return ""
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}
