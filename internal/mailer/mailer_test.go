package mailer

import (
	"bufio"
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"testing"
)

func mapLookup(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

func TestLoadConfigFrom(t *testing.T) {
	cfg, err := LoadConfigFrom(mapLookup(nil))
	if err != nil {
		t.Fatalf("empty env error = %v", err)
	}
	if NewSMTPSender(cfg).Enabled() {
		t.Fatal("expected disabled sender when GNVIPI_SMTP_HOST is unset")
	}

	cfg, err = LoadConfigFrom(mapLookup(map[string]string{
		"GNVIPI_SMTP_HOST":     "smtp.example.com",
		"GNVIPI_SMTP_USER":     "noreply@example.com",
		"GNVIPI_SMTP_PASSWORD": "secret",
	}))
	if err != nil {
		t.Fatalf("valid env error = %v", err)
	}
	if cfg.Port != 587 || cfg.From != "noreply@example.com" {
		t.Fatalf("unexpected cfg: %+v", cfg)
	}
	if !NewSMTPSender(cfg).Enabled() {
		t.Fatal("expected enabled sender")
	}

	if _, err := LoadConfigFrom(mapLookup(map[string]string{
		"GNVIPI_SMTP_HOST": "smtp.example.com",
		"GNVIPI_SMTP_PORT": "99999",
		"GNVIPI_SMTP_FROM": "noreply@example.com",
	})); err == nil {
		t.Fatal("expected invalid port error")
	}

	if _, err := LoadConfigFrom(mapLookup(map[string]string{
		"GNVIPI_SMTP_HOST": "smtp.example.com",
	})); err == nil {
		t.Fatal("expected missing sender error")
	}
}

func TestSMTPSenderDisabledAndHeaderSanitization(t *testing.T) {
	if err := NewSMTPSender(Config{}).Send(context.Background(), "user@example.com", "hi", "body"); !errors.Is(err, ErrDisabled) {
		t.Fatalf("err = %v, want ErrDisabled", err)
	}

	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := ln.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("close listener: %v", err)
		}
	})

	received := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() {
			if closeErr := conn.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
				t.Errorf("close conn: %v", closeErr)
			}
		}()
		r := bufio.NewReader(conn)
		write := func(s string) {
			if _, wErr := conn.Write([]byte(s + "\r\n")); wErr != nil {
				t.Errorf("write smtp response: %v", wErr)
			}
		}
		write("220 localhost ESMTP")
		var dataBuf strings.Builder
		inData := false
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				break
			}
			trimmed := strings.TrimRight(line, "\r\n")
			if inData {
				if trimmed == "." {
					write("250 OK")
					inData = false
					continue
				}
				dataBuf.WriteString(line)
				continue
			}
			upper := strings.ToUpper(trimmed)
			switch {
			case strings.HasPrefix(upper, "EHLO"), strings.HasPrefix(upper, "HELO"):
				write("250 localhost")
			case strings.HasPrefix(upper, "MAIL FROM:"), strings.HasPrefix(upper, "RCPT TO:"):
				write("250 OK")
			case upper == "DATA":
				write("354 End data with <CR><LF>.<CR><LF>")
				inData = true
			case upper == "QUIT":
				write("221 Bye")
				received <- dataBuf.String()
				return
			default:
				write("250 OK")
			}
		}
	}()

	_, portStr, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}
	sender := NewSMTPSender(Config{
		Host: "127.0.0.1",
		Port: port,
		From: "Gateway <noreply@example.com>",
	})
	err = sender.Send(context.Background(), "alice@example.com", "Your key\r\nBcc: evil@example.com", "line1\nline2")
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	msg := <-received
	if strings.Contains(msg, "\r\nBcc:") {
		t.Fatalf("header injection survived sanitization:\n%s", msg)
	}
	if !strings.Contains(msg, "Subject: Your key  Bcc: evil@example.com\r\n") {
		t.Fatalf("unexpected subject in message:\n%s", msg)
	}
	if !strings.Contains(msg, "line1\r\nline2\r\n") {
		t.Fatalf("unexpected body in message:\n%s", msg)
	}
}
