package notify

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"net/textproto"
	"strconv"
	"strings"
	"testing"
	"time"
)

type smtpMode int

const (
	smtpStartTLS smtpMode = iota
	smtpPlainOnly
	smtpImplicitTLS
	smtpSilent
)

// fakeSMTP is a minimal SMTP submission server for tests. It records
// the DATA payload of each delivered message.
type fakeSMTP struct {
	ln     net.Listener
	mode   smtpMode
	tlsCfg *tls.Config
	roots  *x509.CertPool
	data   chan string
}

func startFakeSMTP(t *testing.T, mode smtpMode) *fakeSMTP {
	t.Helper()
	cert, roots := selfSignedCert(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f := &fakeSMTP{ln: ln, mode: mode, roots: roots,
		tlsCfg: &tls.Config{Certificates: []tls.Certificate{cert}},
		data:   make(chan string, 4)}
	t.Cleanup(func() { _ = ln.Close() })
	go f.acceptLoop()
	return f
}

func (f *fakeSMTP) port() string {
	return strconv.Itoa(f.ln.Addr().(*net.TCPAddr).Port)
}

func (f *fakeSMTP) acceptLoop() {
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			return
		}
		go f.serve(conn)
	}
}

func (f *fakeSMTP) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	if f.mode == smtpSilent {
		time.Sleep(5 * time.Second)
		return
	}
	if f.mode == smtpImplicitTLS {
		conn = tls.Server(conn, f.tlsCfg)
	}
	tp := textproto.NewConn(conn)
	_ = tp.PrintfLine("220 fake ESMTP")
	secure := f.mode == smtpImplicitTLS
	for {
		line, err := tp.ReadLine()
		if err != nil {
			return
		}
		verb := strings.ToUpper(strings.Fields(line + " x")[0])
		switch verb {
		case "EHLO", "HELO":
			if f.mode == smtpStartTLS && !secure {
				_ = tp.PrintfLine("250-fake\r\n250-STARTTLS\r\n250 AUTH PLAIN")
			} else {
				_ = tp.PrintfLine("250-fake\r\n250 AUTH PLAIN")
			}
		case "STARTTLS":
			_ = tp.PrintfLine("220 ready")
			conn = tls.Server(conn, f.tlsCfg)
			tp = textproto.NewConn(conn)
			secure = true
		case "AUTH":
			_ = tp.PrintfLine("235 ok")
		case "DATA":
			_ = tp.PrintfLine("354 go")
			body, _ := tp.ReadDotBytes()
			f.data <- string(body)
			_ = tp.PrintfLine("250 ok")
		case "QUIT":
			_ = tp.PrintfLine("221 bye")
			return
		default:
			_ = tp.PrintfLine("250 ok")
		}
	}
}

func selfSignedCert(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "fake-smtp"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl,
		&key.PublicKey, key)
	if err != nil {
		t.Fatalf("cert: %v", err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(parsed)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, roots
}
