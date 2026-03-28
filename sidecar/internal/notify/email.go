package notify

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/smtp"
	"strings"
)

// EmailSender delivers notifications via SMTP with TLS.
type EmailSender struct{}

// NewEmailSender creates an EmailSender.
func NewEmailSender() *EmailSender { return &EmailSender{} }

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
	return sendEmail(ctx, cfg, evt)
}

type emailConfig struct {
	Host string
	Port string
	User string
	Pass string
	From string
	To   []string
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

	return &emailConfig{
		Host: host,
		Port: port,
		User: ch.Config["smtp_user"],
		Pass: ch.Config["smtp_pass"],
		From: from,
		To:   splitRecipients(to),
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

func sendEmail(
	_ context.Context, cfg *emailConfig, evt Event,
) error {
	addr := net.JoinHostPort(cfg.Host, cfg.Port)

	tlsCfg := &tls.Config{
		ServerName: cfg.Host,
		MinVersion: tls.VersionTLS12,
	}
	conn, err := tls.Dial("tcp", addr, tlsCfg)
	if err != nil {
		return fmt.Errorf("tls dial %s: %w", addr, err)
	}

	client, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("smtp client: %w", err)
	}
	defer client.Close()

	if err := authenticateSMTP(client, cfg); err != nil {
		return err
	}

	if err := setSMTPEnvelope(client, cfg); err != nil {
		return err
	}

	return writeMessage(client, cfg, evt)
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
		wc.Close()
		return fmt.Errorf("smtp write body: %w", err)
	}

	if err := wc.Close(); err != nil {
		return fmt.Errorf("smtp close data: %w", err)
	}
	return client.Quit()
}

func formatEmailMessage(
	cfg *emailConfig, evt Event,
) string {
	return fmt.Sprintf(
		"From: %s\r\nTo: %s\r\nSubject: [pg_sage] %s\r\n"+
			"Content-Type: text/plain; charset=UTF-8\r\n\r\n%s\r\n"+
			"\r\nEvent: %s\r\nSeverity: %s\r\n",
		cfg.From,
		strings.Join(cfg.To, ", "),
		evt.Subject,
		evt.Body,
		evt.Type,
		evt.Severity,
	)
}
