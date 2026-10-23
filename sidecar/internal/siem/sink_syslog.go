package siem

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"time"
)

// syslogFacility is "log audit" (RFC 5424 facility 13).
const syslogFacility = 13

type syslogSink struct {
	name, network, address string
	timeout                time.Duration
	tlsCfg                 *tls.Config
	hostname               string

	mu   sync.Mutex
	conn net.Conn
}

// NewSyslogSink sends one RFC 5424 message per event: over tcp or tls with
// octet-counting framing (RFC 6587, RFC 5425), over udp one per datagram.
// tlsCfg nil verifies the receiver against the system roots.
func NewSyslogSink(name, network, address string, timeout time.Duration,
	tlsCfg *tls.Config) Sink {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "-"
	}
	if network == "" {
		network = "tcp"
	}
	return &syslogSink{name: name, network: network, address: address, timeout: timeout,
		tlsCfg: tlsCfg, hostname: host}
}

func (s *syslogSink) Name() string { return s.name }

func (s *syslogSink) Send(ctx context.Context, events []Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range events {
		if err := ctx.Err(); err != nil {
			return err
		}
		msg, err := s.format(e)
		if err != nil {
			return fmt.Errorf("siem sink %s: encode: %w", s.name, err)
		}
		if err := s.write(ctx, msg); err != nil {
			s.closeLocked()
			return fmt.Errorf("siem sink %s: deliver: %w", s.name, err)
		}
	}
	return nil
}

// format renders e as RFC 5424: <PRI>1 TIMESTAMP HOST APP PROCID MSGID SD MSG,
// with the OCSF class as MSGID and the event JSON as MSG.
func (s *syslogSink) format(e Event) ([]byte, error) {
	body, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}
	ms, _ := e["time"].(int64)
	sev, _ := e["severity_id"].(int)
	class, _ := e["class_uid"].(int)
	ts := time.UnixMilli(ms).UTC().Format("2006-01-02T15:04:05.000Z07:00")
	head := fmt.Sprintf("<%d>1 %s %s pg_sage %d %d - ", syslogFacility*8+syslogSeverity(sev),
		ts, s.hostname, os.Getpid(), class)
	return append([]byte(head), body...), nil
}

func syslogSeverity(ocsf int) int {
	switch ocsf {
	case SeverityMedium:
		return 4 // warning
	case SeverityHigh:
		return 3 // error
	}
	return 6 // informational
}

func (s *syslogSink) write(ctx context.Context, msg []byte) error {
	if s.conn != nil && s.network != "udp" && !peerAlive(s.conn) {
		s.closeLocked()
	}
	if s.conn == nil {
		conn, err := s.dial(ctx)
		if err != nil {
			return err
		}
		s.conn = conn
	}
	if err := s.conn.SetWriteDeadline(time.Now().Add(s.timeout)); err != nil {
		return err
	}
	if s.network != "udp" {
		msg = append([]byte(fmt.Sprintf("%d ", len(msg))), msg...)
	}
	_, err := s.conn.Write(msg)
	return err
}

func (s *syslogSink) dial(ctx context.Context) (net.Conn, error) {
	d := &net.Dialer{Timeout: s.timeout}
	switch s.network {
	case "tls":
		cfg := s.tlsCfg
		if cfg == nil {
			cfg = &tls.Config{MinVersion: tls.VersionTLS12}
		}
		td := &tls.Dialer{NetDialer: d, Config: cfg}
		return td.DialContext(ctx, "tcp", s.address)
	case "udp", "tcp":
		return d.DialContext(ctx, s.network, s.address)
	}
	return nil, fmt.Errorf("unsupported syslog network %q", s.network)
}

// peerAlive probes a stream connection before reuse: a syslog receiver
// never writes, so a read that times out means the peer is still there,
// and EOF or a reset means it closed (a write would vanish into the
// kernel buffer and be lost).
func peerAlive(conn net.Conn) bool {
	if err := conn.SetReadDeadline(time.Now().Add(time.Millisecond)); err != nil {
		return false
	}
	var one [1]byte
	_, err := conn.Read(one[:])
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func (s *syslogSink) closeLocked() {
	if s.conn != nil {
		_ = s.conn.Close()
		s.conn = nil
	}
}

// Close closes the connection, if any.
func (s *syslogSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeLocked()
	return nil
}
