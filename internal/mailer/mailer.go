// Package mailer provides SMTP email delivery using the Go standard library.
package mailer

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

const (
	defaultSMTPPort = 587
	dialTimeout     = 10 * time.Second
)

// ErrDisabled is returned when SMTP is not configured.
var ErrDisabled = errors.New("mailer: SMTP is not configured")

// Config holds SMTP connection and sender settings loaded from environment variables.
type Config struct {
	Host     string
	Port     int
	User     string
	Password string
	From     string
}

// Sender sends plain-text emails.
type Sender interface {
	Enabled() bool
	Send(ctx context.Context, to, subject, body string) error
}

// LoadConfigFrom reads SMTP configuration using lookup (typically os.LookupEnv).
// When GNVIPI_SMTP_HOST is empty, SMTP is disabled and a zero Config is returned.
func LoadConfigFrom(lookup func(string) (string, bool)) (Config, error) {
	get := func(k string) string {
		v, _ := lookup(k)
		return strings.TrimSpace(v)
	}
	host := get("GNVIPI_SMTP_HOST")
	if host == "" {
		return Config{}, nil
	}
	port := defaultSMTPPort
	if rawPort := get("GNVIPI_SMTP_PORT"); rawPort != "" {
		p, err := strconv.Atoi(rawPort)
		if err != nil || p < 1 || p > 65535 {
			return Config{}, fmt.Errorf("mailer: invalid GNVIPI_SMTP_PORT %q", rawPort)
		}
		port = p
	}
	user := get("GNVIPI_SMTP_USER")
	password, _ := lookup("GNVIPI_SMTP_PASSWORD")
	from := get("GNVIPI_SMTP_FROM")
	if from == "" {
		from = user
	}
	if from == "" {
		return Config{}, errors.New("mailer: GNVIPI_SMTP_FROM or GNVIPI_SMTP_USER is required when GNVIPI_SMTP_HOST is set")
	}
	if _, err := mail.ParseAddress(from); err != nil {
		return Config{}, fmt.Errorf("mailer: invalid sender address %q: %w", from, err)
	}
	return Config{
		Host:     host,
		Port:     port,
		User:     user,
		Password: password,
		From:     from,
	}, nil
}

// SMTPSender delivers email over SMTP using net/smtp and crypto/tls.
type SMTPSender struct {
	cfg       Config
	tlsConfig *tls.Config
}

// NewSMTPSender constructs an SMTPSender from cfg.
func NewSMTPSender(cfg Config) *SMTPSender {
	return &SMTPSender{cfg: cfg}
}

// Enabled reports whether an SMTP host is configured.
func (s *SMTPSender) Enabled() bool {
	return s != nil && s.cfg.Host != ""
}

// Send delivers a plain-text email to the recipient address.
func (s *SMTPSender) Send(ctx context.Context, to, subject, body string) error {
	if !s.Enabled() {
		return ErrDisabled
	}
	rcpt, err := mail.ParseAddress(strings.TrimSpace(to))
	if err != nil {
		return fmt.Errorf("mailer: invalid recipient address: %w", err)
	}
	fromAddr, err := mail.ParseAddress(s.cfg.From)
	if err != nil {
		return fmt.Errorf("mailer: invalid sender address: %w", err)
	}
	cleanSubject := sanitizeHeader(subject)
	addr := net.JoinHostPort(s.cfg.Host, strconv.Itoa(s.cfg.Port))
	dialer := &net.Dialer{Timeout: dialTimeout}

	var conn net.Conn
	if s.cfg.Port == 465 {
		tlsCfg := s.tlsCfg()
		conn, err = (&tls.Dialer{NetDialer: dialer, Config: tlsCfg}).DialContext(ctx, "tcp", addr)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("mailer: dial %s: %w", addr, err)
	}

	client, err := smtp.NewClient(conn, s.cfg.Host)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("mailer: smtp client: %w", err)
	}
	defer func() { _ = client.Close() }()

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	if s.cfg.Port != 465 {
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(s.tlsCfg()); err != nil {
				return fmt.Errorf("mailer: starttls: %w", err)
			}
		}
	}

	if s.cfg.User != "" {
		auth := smtp.PlainAuth("", s.cfg.User, s.cfg.Password, s.cfg.Host)
		if err := client.Auth(auth); err != nil {
			return fmt.Errorf("mailer: auth: %w", err)
		}
	}

	if err := client.Mail(fromAddr.Address); err != nil {
		return fmt.Errorf("mailer: mail from: %w", err)
	}
	if err := client.Rcpt(rcpt.Address); err != nil {
		return fmt.Errorf("mailer: rcpt to: %w", err)
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("mailer: data: %w", err)
	}
	msg := buildMessage(fromAddr.String(), rcpt.Address, cleanSubject, body)
	if _, err := w.Write([]byte(msg)); err != nil {
		_ = w.Close()
		return fmt.Errorf("mailer: write body: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("mailer: close data: %w", err)
	}
	if err := client.Quit(); err != nil {
		return fmt.Errorf("mailer: quit: %w", err)
	}
	return nil
}

func (s *SMTPSender) tlsCfg() *tls.Config {
	if s.tlsConfig != nil {
		return s.tlsConfig
	}
	return &tls.Config{ServerName: s.cfg.Host, MinVersion: tls.VersionTLS12}
}

func sanitizeHeader(v string) string {
	v = strings.ReplaceAll(v, "\r", " ")
	v = strings.ReplaceAll(v, "\n", " ")
	return strings.TrimSpace(v)
}

func buildMessage(from, to, subject, body string) string {
	normalizedBody := strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\n", "\r\n")
	return fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\n%s\r\n",
		from, to, subject, normalizedBody)
}
