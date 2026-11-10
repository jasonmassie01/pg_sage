package siem

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/pg-sage/sidecar/internal/egress"
)

// TLSFiles names a sink's PEM files: CA pins the receiver's roots to that
// bundle (empty: the system roots); Cert and Key are the client pair for
// mutual TLS.
type TLSFiles struct {
	CA, Cert, Key string
}

// LoadTLSConfig reads the files once and returns the sink's client TLS
// configuration: TLS 1.2 at least, the CA bundle alone as roots when CA
// is set, the client pair when Cert and Key are. Errors name the file.
func LoadTLSConfig(f TLSFiles) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if f.CA != "" {
		pemBytes, err := os.ReadFile(f.CA)
		if err != nil {
			return nil, fmt.Errorf("ca_file %s: %w", f.CA, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pemBytes) {
			return nil, fmt.Errorf("ca_file %s holds no PEM certificate", f.CA)
		}
		cfg.RootCAs = pool
	}
	if (f.Cert == "") != (f.Key == "") {
		return nil, errors.New("client_cert_file and client_key_file must be set together")
	}
	if f.Cert != "" {
		pair, err := tls.LoadX509KeyPair(f.Cert, f.Key)
		if err != nil {
			return nil, fmt.Errorf("client_cert_file %s / client_key_file %s: %w", f.Cert,
				f.Key, err)
		}
		cfg.Certificates = []tls.Certificate{pair}
	}
	return cfg, nil
}

// httpClient is a client with the sink's timeout on the egress floor
// (security-03); tlsCfg nil keeps the default TLS settings (system roots,
// SSL_CERT_FILE where Go honours it).
func httpClient(timeout time.Duration, tlsCfg *tls.Config) *http.Client {
	client := egress.HTTPClient(timeout)
	if tlsCfg == nil {
		return client
	}
	tr := egress.Transport(timeout)
	tr.TLSClientConfig = tlsCfg.Clone()
	client.Transport = tr
	return client
}

// tlsConfirmWait bounds how long a new TLS 1.3 syslog session waits for
// the receiver to refuse the client certificate it asked for.
const tlsConfirmWait = 500 * time.Millisecond

// dialTLS dials a TLS syslog receiver. Under TLS 1.3 the client finishes
// its handshake before the receiver checks the client certificate, so a
// refusal arrives only as an alert afterwards and the first message would
// be written into a closing session and lost. When the receiver asked for
// a certificate, wait briefly for that alert before using the session.
func dialTLS(ctx context.Context, d *net.Dialer, address string, base *tls.Config,
	wait time.Duration) (net.Conn, error) {
	cfg := base.Clone()
	requested := false
	pairs := cfg.Certificates
	cfg.GetClientCertificate = func(cri *tls.CertificateRequestInfo) (*tls.Certificate,
		error) {
		requested = true
		for i := range pairs {
			if cri.SupportsCertificate(&pairs[i]) == nil {
				return &pairs[i], nil
			}
		}
		return &tls.Certificate{}, nil
	}
	conn, err := (&tls.Dialer{NetDialer: d, Config: cfg}).DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}
	tc := conn.(*tls.Conn)
	if !requested || tc.ConnectionState().Version < tls.VersionTLS13 {
		return conn, nil
	}
	if err := confirmSession(tc, wait); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("the receiver refused the TLS session: %w", err)
	}
	return conn, nil
}

// confirmSession reads for wait: a syslog receiver never writes, so a
// timeout means it kept the session; an alert or EOF means it refused it.
func confirmSession(conn *tls.Conn, wait time.Duration) error {
	if err := conn.SetReadDeadline(time.Now().Add(wait)); err != nil {
		return err
	}
	var one [1]byte
	_, err := conn.Read(one[:])
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return conn.SetReadDeadline(time.Time{})
	}
	if err == nil {
		return errors.New("the receiver wrote before any message was sent")
	}
	return err
}
