package notify

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"strings"
	"time"
)

// defaultSMTPTimeout bounds the whole SMTP conversation so a stalled
// peer cannot block the analyzer/executor loop (G7-B16).
const defaultSMTPTimeout = 30 * time.Second

// EmailSender delivers notifications via SMTP. Port 465 (or
// smtp_tls=implicit) uses implicit TLS; any other port requires
// STARTTLS and fails closed when the server does not offer it (G7-B08).
type EmailSender struct {
	policy  TargetPolicy
	tlsBase *tls.Config // optional; cloned per send (tests inject roots)
	timeout time.Duration
}

// NewEmailSender creates an EmailSender that refuses internal hosts.
func NewEmailSender() *EmailSender { return NewEmailSenderWithPolicy(TargetPolicy{}) }

// NewEmailSenderWithPolicy creates an EmailSender with an explicit
// target policy (AllowPrivate for internal relays).
func NewEmailSenderWithPolicy(policy TargetPolicy) *EmailSender {
	return &EmailSender{policy: policy, timeout: defaultSMTPTimeout}
}

// Type returns the channel type identifier.
func (e *EmailSender) Type() string { return "email" }

// Send delivers an email via SMTP using channel config.
func (e *EmailSender) Send(
	ctx context.Context, ch Channel, evt Event,
) error {
	cfg, err := parseEmailConfig(ch)
	if err != nil {
		return err
	}
	if err := e.policy.ValidateHost(cfg.Host); err != nil {
		return fmt.Errorf("email channel %q: %w", ch.Name, err)
	}
	return e.sendEmail(ctx, cfg, evt)
}

type emailConfig struct {
	Host        string
	Port        string
	User        string
	Pass        string
	From        string
	To          []string
	ImplicitTLS bool
}

func parseEmailConfig(ch Channel) (*emailConfig, error) {
	host := ch.Config["smtp_host"]
	if host == "" {
		return nil, fmt.Errorf(
			"email channel %q: missing smtp_host", ch.Name)
	}
	from := ch.Config["from"]
	if from == "" {
		return nil, fmt.Errorf(
			"email channel %q: missing from", ch.Name)
	}
	to := ch.Config["to"]
	if to == "" {
		return nil, fmt.Errorf(
			"email channel %q: missing to", ch.Name)
	}

	port := ch.Config["smtp_port"]
	if port == "" {
		port = "587"
	}
	mode := strings.ToLower(ch.Config["smtp_tls"])

	return &emailConfig{
		Host:        host,
		Port:        port,
		User:        ch.Config["smtp_user"],
		Pass:        ch.Config["smtp_pass"],
		From:        from,
		To:          splitRecipients(to),
		ImplicitTLS: mode == "implicit" || (mode == "" && port == "465"),
	}, nil
}

func splitRecipients(to string) []string {
	parts := strings.Split(to, ",")
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		trimmed := strings.TrimSpace(p)
		if trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

func (e *EmailSender) tlsConfig(host string) *tls.Config {
	cfg := &tls.Config{}
	if e.tlsBase != nil {
		cfg = e.tlsBase.Clone()
	}
	cfg.ServerName = host
	cfg.MinVersion = tls.VersionTLS12
	return cfg
}

func (e *EmailSender) sendEmail(
	ctx context.Context, cfg *emailConfig, evt Event,
) error {
	timeout := e.timeout
	if timeout <= 0 {
		timeout = defaultSMTPTimeout
	}
	client, err := e.connect(ctx, cfg, timeout)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	if err := authenticateSMTP(client, cfg); err != nil {
		return err
	}
	if err := setSMTPEnvelope(client, cfg); err != nil {
		return err
	}
	return writeMessage(client, cfg, evt)
}

// connect dials with a timeout, bounds the conversation with a deadline
// derived from ctx, and establishes TLS (implicit or STARTTLS).
func (e *EmailSender) connect(
	ctx context.Context, cfg *emailConfig, timeout time.Duration,
) (*smtp.Client, error) {
	addr := net.JoinHostPort(cfg.Host, cfg.Port)
	conn, err := e.policy.dialContext(ctx, timeout, addr)
	if err != nil {
		return nil, fmt.Errorf("smtp dial %s: %w", addr, err)
	}
	deadline := time.Now().Add(timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := conn.SetDeadline(deadline); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("smtp set deadline: %w", err)
	}
	if cfg.ImplicitTLS {
		conn = tls.Client(conn, e.tlsConfig(cfg.Host))
	}
	client, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("smtp client: %w", err)
	}
	if cfg.ImplicitTLS {
		return client, nil
	}
	return client, e.startTLS(client, cfg)
}

func (e *EmailSender) startTLS(client *smtp.Client, cfg *emailConfig) error {
	if ok, _ := client.Extension("STARTTLS"); !ok {
		_ = client.Close()
		return fmt.Errorf("smtp server %s does not offer STARTTLS; "+
			"refusing to send credentials in plaintext "+
			"(use port 465 or smtp_tls=implicit for implicit TLS)", cfg.Host)
	}
	if err := client.StartTLS(e.tlsConfig(cfg.Host)); err != nil {
		_ = client.Close()
		return fmt.Errorf("smtp STARTTLS: %w", err)
	}
	return nil
}

func authenticateSMTP(
	client *smtp.Client, cfg *emailConfig,
) error {
	if cfg.User == "" {
		return nil
	}
	auth := smtp.PlainAuth("", cfg.User, cfg.Pass,
		cfg.Host)
	if err := client.Auth(auth); err != nil {
		return fmt.Errorf("smtp auth: %w", err)
	}
	return nil
}

func setSMTPEnvelope(
	client *smtp.Client, cfg *emailConfig,
) error {
	if err := client.Mail(cfg.From); err != nil {
		return fmt.Errorf("smtp MAIL FROM: %w", err)
	}
	for _, to := range cfg.To {
		if err := client.Rcpt(to); err != nil {
			return fmt.Errorf("smtp RCPT TO %s: %w", to, err)
		}
	}
	return nil
}

func writeMessage(
	client *smtp.Client, cfg *emailConfig, evt Event,
) error {
	wc, err := client.Data()
	if err != nil {
		return fmt.Errorf("smtp DATA: %w", err)
	}

	msg := formatEmailMessage(cfg, evt)
	if _, err := wc.Write([]byte(msg)); err != nil {
		_ = wc.Close()
		return fmt.Errorf("smtp write body: %w", err)
	}

	if err := wc.Close(); err != nil {
		return fmt.Errorf("smtp close data: %w", err)
	}
	return client.Quit()
}

// formatEmailMessage renders RFC 5322 headers from sanitized values:
// CR/LF in subjects (identifiers can contain newlines) must not inject
// headers (G7-B30); non-ASCII subjects are Q-encoded.
func formatEmailMessage(
	cfg *emailConfig, evt Event,
) string {
	subject := mime.QEncoding.Encode("UTF-8",
		sanitizeHeader("[pg_sage] "+evt.Subject))
	return fmt.Sprintf(
		"From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\n"+
			"Message-ID: <%s@pg-sage>\r\nMIME-Version: 1.0\r\n"+
			"Content-Type: text/plain; charset=UTF-8\r\n\r\n%s\r\n"+
			"\r\nEvent: %s\r\nSeverity: %s\r\n",
		sanitizeHeader(cfg.From),
		sanitizeHeader(strings.Join(cfg.To, ", ")),
		subject,
		time.Now().UTC().Format(time.RFC1123Z),
		messageID(),
		evt.Body,
		evt.Type,
		evt.Severity,
	)
}

func messageID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
