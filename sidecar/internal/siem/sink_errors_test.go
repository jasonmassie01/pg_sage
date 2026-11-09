package siem

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// statusServer answers every request with status.
func statusServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// 400, 413 and 422 refuse the events themselves (ErrPermanent); every
// other failure (auth, rate limit, server errors) is transient, so the
// batch is retried and nothing is stubbed while a receiver is misconfigured.
func TestHTTPStatusClassification(t *testing.T) {
	for _, c := range []struct {
		status    int
		permanent bool
	}{{400, true}, {413, true}, {422, true}, {401, false}, {403, false}, {404, false},
		{408, false}, {409, false}, {429, false}, {500, false}, {502, false},
		{503, false}} {
		srv := statusServer(t, c.status, "")
		for _, s := range []Sink{NewHTTPSink("soc", srv.URL, "tok", time.Second, nil),
			NewOTLPSink("otel", srv.URL+"/v1/logs", "tok", time.Second, nil)} {
			err := s.Send(context.Background(), sampleEvents(2))
			if err == nil || !strings.Contains(err.Error(), strconv.Itoa(c.status)) {
				t.Fatalf("%s HTTP %d: err = %v", s.Name(), c.status, err)
			}
			if errors.Is(err, ErrPermanent) != c.permanent {
				t.Fatalf("%s HTTP %d: permanent = %v, want %v (%v)", s.Name(), c.status,
					!c.permanent, c.permanent, err)
			}
		}
	}
}

// A 2xx is success; an unreachable receiver is transient.
func TestHTTPSuccessAndNetworkErrors(t *testing.T) {
	srv := statusServer(t, http.StatusAccepted, "")
	if err := NewHTTPSink("soc", srv.URL, "", time.Second, nil).Send(context.Background(),
		sampleEvents(1)); err != nil {
		t.Fatalf("202: %v", err)
	}
	err := NewHTTPSink("soc", "http://127.0.0.1:1", "", time.Second, nil).
		Send(context.Background(), sampleEvents(1))
	if err == nil || errors.Is(err, ErrPermanent) {
		t.Fatalf("unreachable receiver: err = %v, want a transient error", err)
	}
}

// OTLP rejected records are permanent; a reply that rejects none, or one
// that is not OTLP JSON, is success.
func TestOTLPRejectionClassification(t *testing.T) {
	for _, c := range []struct {
		body      string
		permanent bool
		ok        bool
	}{{`{"partialSuccess":{"rejectedLogRecords":"2","errorMessage":"too big"}}`, true, false},
		{`{"partialSuccess":{"rejectedLogRecords":3}}`, true, false},
		{`{"partialSuccess":{"rejectedLogRecords":"0"}}`, false, true},
		{`{"partialSuccess":{}}`, false, true},
		{`not json`, false, true}, {``, false, true}} {
		srv := statusServer(t, http.StatusOK, c.body)
		err := NewOTLPSink("otel", srv.URL+"/v1/logs", "", time.Second, nil).
			Send(context.Background(), sampleEvents(3))
		if c.ok != (err == nil) || errors.Is(err, ErrPermanent) != c.permanent {
			t.Fatalf("reply %q: err = %v, want ok %v permanent %v", c.body, err, c.ok,
				c.permanent)
		}
	}
}

// An event too large for one syslog message is permanent and nothing of
// it is written; the events before it are sent.
func TestSyslogOversizeEventIsPermanent(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer pc.Close()
	s := NewSyslogSink("rsys", "udp", pc.LocalAddr().String(), time.Second, nil)
	defer s.(io.Closer).Close()
	events := sampleEvents(2)
	events[1]["message"] = strings.Repeat("x", MaxSyslogBytes)
	err = s.Send(context.Background(), events)
	if !errors.Is(err, ErrPermanent) || !strings.Contains(err.Error(), "rsys") {
		t.Fatalf("oversize event: err = %v, want ErrPermanent naming the sink", err)
	}
	buf := make([]byte, 70000)
	_ = pc.SetReadDeadline(time.Now().Add(2 * time.Second))
	if n, _, err := pc.ReadFrom(buf); err != nil || !syslogHeader.Match(buf[:n]) {
		t.Fatalf("the event before the oversize one: %q (%v)", buf[:n], err)
	}
	_ = pc.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if n, _, err := pc.ReadFrom(buf); err == nil {
		t.Fatalf("the oversize event was written (%d bytes)", n)
	}
	events[1]["message"] = strings.Repeat("x", 1000)
	if err := s.Send(context.Background(), events[1:]); err != nil {
		t.Fatalf("a normal event after the oversize one: %v", err)
	}
}

// A syslog receiver that cannot be reached is transient.
func TestSyslogDialFailureIsTransient(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	err = NewSyslogSink("rsys", "tcp", addr, 200*time.Millisecond, nil).
		Send(context.Background(), sampleEvents(1))
	if err == nil || errors.Is(err, ErrPermanent) {
		t.Fatalf("closed port: err = %v, want a transient error", err)
	}
}

// An event that cannot be encoded is permanent: no retry can encode it.
func TestUnencodableEventIsPermanent(t *testing.T) {
	srv := statusServer(t, http.StatusOK, "")
	bad := sampleEvents(1)
	bad[0]["message"] = make(chan int)
	for _, s := range []Sink{NewHTTPSink("soc", srv.URL, "", time.Second, nil),
		NewOTLPSink("otel", srv.URL+"/v1/logs", "", time.Second, nil)} {
		if err := s.Send(context.Background(), bad); !errors.Is(err, ErrPermanent) {
			t.Fatalf("%s: unencodable event: err = %v, want ErrPermanent", s.Name(), err)
		}
	}
}
