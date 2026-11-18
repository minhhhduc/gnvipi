// Package mailer provides SMTP email delivery using the Go standard library.
package mailer

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"net/url"
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

// Config holds SMTP and optional Cloudflare Mail Worker settings loaded from
// environment variables.
type Config struct {
	Host      string
	Port      int
	User      string
	Password  string
	From      string
	WorkerURL string
}

// Sender sends plain-text emails.
type Sender interface {
	Enabled() bool
	Send(ctx context.Context, to, subject, body string) error
}

// LoadConfigFrom reads email configuration using lookup (typically os.LookupEnv).
// When both GNVIPI_SMTP_HOST and GNVIPI_CF_MAIL_WORKER_URL are empty, delivery
// is disabled and a zero Config is returned.
func LoadConfigFrom(lookup func(string) (string, bool)) (Config, error) {
	get := func(k string) string {
		v, _ := lookup(k)
		return strings.TrimSpace(v)
	}
	host := get("GNVIPI_SMTP_HOST")
	workerURL := get("GNVIPI_CF_MAIL_WORKER_URL")
	if host == "" && workerURL == "" {
		return Config{}, nil
	}
	if workerURL != "" {
		u, err := url.Parse(workerURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return Config{}, fmt.Errorf("mailer: invalid GNVIPI_CF_MAIL_WORKER_URL %q", workerURL)
		}
	}
	port := defaultSMTPPort
	if host != "" {
		if rawPort := get("GNVIPI_SMTP_PORT"); rawPort != "" {
			p, err := strconv.Atoi(rawPort)
			if err != nil || p < 1 || p > 65535 {
				return Config{}, fmt.Errorf("mailer: invalid GNVIPI_SMTP_PORT %q", rawPort)
			}
			port = p
		}
	}
	user := get("GNVIPI_SMTP_USER")
	password, _ := lookup("GNVIPI_SMTP_PASSWORD")
	from := get("GNVIPI_SMTP_FROM")
	if from == "" {
		from = user
	}
	if host != "" && from == "" {
		return Config{}, errors.New("mailer: GNVIPI_SMTP_FROM or GNVIPI_SMTP_USER is required when GNVIPI_SMTP_HOST is set")
	}
	if from != "" {
		if _, err := mail.ParseAddress(from); err != nil {
			return Config{}, fmt.Errorf("mailer: invalid sender address %q: %w", from, err)
		}
	}
	return Config{
		Host:      host,
		Port:      port,
		User:      user,
		Password:  password,
		From:      from,
		WorkerURL: workerURL,
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

// Enabled reports whether an SMTP host or mail worker URL is configured.
func (s *SMTPSender) Enabled() bool {
	return s != nil && (s.cfg.WorkerURL != "" || s.cfg.Host != "")
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
	cleanSubject := sanitizeHeader(subject)
	if s.cfg.WorkerURL != "" {
		return s.sendViaWorker(ctx, rcpt.Address, cleanSubject, body)
	}
	fromAddr, err := mail.ParseAddress(s.cfg.From)
	if err != nil {
		return fmt.Errorf("mailer: invalid sender address: %w", err)
	}
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
		return errors.Join(fmt.Errorf("mailer: smtp client: %w", err), conn.Close())
	}
	defer func() { _ = client.Close() }() //nolint:errcheck // best-effort connection teardown

	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return fmt.Errorf("mailer: set deadline: %w", err)
		}
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
		return errors.Join(fmt.Errorf("mailer: write body: %w", err), w.Close())
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("mailer: close data: %w", err)
	}
	if err := client.Quit(); err != nil {
		return fmt.Errorf("mailer: quit: %w", err)
	}
	return nil
}

func (s *SMTPSender) sendViaWorker(ctx context.Context, to, subject, body string) error {
	msg := map[string]string{
		"to":      to,
		"subject": subject,
		"text":    body,
		"body":    body,
	}
	if s.cfg.From != "" {
		msg["from"] = s.cfg.From
	}
	payload, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("mailer: encode worker payload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.WorkerURL, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("mailer: build worker request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if s.cfg.Password != "" {
		req.Header.Set("Authorization", "Bearer "+s.cfg.Password)
	}
	resp, err := (&http.Client{Timeout: dialTimeout}).Do(req)
	if err != nil {
		return fmt.Errorf("mailer: worker request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort response body close
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if readErr != nil {
			return fmt.Errorf("mailer: worker status %d (read body: %w)", resp.StatusCode, readErr)
		}
		return fmt.Errorf("mailer: worker status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
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
