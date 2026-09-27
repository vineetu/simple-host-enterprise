package handler

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"net/url"
	"strings"
	"time"
)

// SMTPMailer sends plain-text mail through SMTP_URL: smtp:// requires
// STARTTLS (and then authenticates if the URL carries a user) unless the URL
// ends in ?insecure=1, smtps:// is TLS from the first byte. Nothing else in
// the product sends mail; it exists for the idle-site notice.
type SMTPMailer struct {
	url *url.URL
	// from is SMTP_FROM for the From header (a display name allowed);
	// envelope is its bare address, for MAIL FROM.
	from     string
	envelope string
	insecure bool
}

// NewSMTPMailer parses SMTP_URL and SMTP_FROM (already validated by config).
func NewSMTPMailer(rawURL, from string) (*SMTPMailer, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	sender, err := mail.ParseAddress(from)
	if err != nil {
		return nil, err
	}
	return &SMTPMailer{url: u, from: sender.String(), envelope: sender.Address, insecure: u.Query().Get("insecure") == "1"}, nil
}

// Send delivers one message to every address in to, within 30 seconds.
func (m *SMTPMailer) Send(ctx context.Context, to []string, subject, body string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	host := m.url.Hostname()
	port := m.url.Port()
	if port == "" {
		port = "587"
		if m.url.Scheme == "smtps" {
			port = "465"
		}
	}
	dialer := &net.Dialer{}
	var conn net.Conn
	var err error
	if m.url.Scheme == "smtps" {
		conn, err = (&tls.Dialer{NetDialer: dialer, Config: &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}}).DialContext(ctx, "tcp", net.JoinHostPort(host, port))
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, port))
	}
	if err != nil {
		return err
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	client, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return err
	}
	defer client.Close()
	if m.url.Scheme == "smtp" {
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}); err != nil {
				return err
			}
		} else if !m.insecure {
			return errors.New("the SMTP server does not offer STARTTLS; use smtps://, or add ?insecure=1 to SMTP_URL to send without TLS")
		}
	}
	if user := m.url.User; user != nil {
		password, _ := user.Password()
		if err := client.Auth(smtp.PlainAuth("", user.Username(), password, host)); err != nil {
			return err
		}
	}
	if err := client.Mail(m.envelope); err != nil {
		return err
	}
	for _, addr := range to {
		if err := client.Rcpt(addr); err != nil {
			return err
		}
	}
	w, err := client.Data()
	if err != nil {
		return err
	}
	msg := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n%s",
		m.from, strings.Join(to, ", "), mime.QEncoding.Encode("utf-8", headerSafe(subject)), time.Now().UTC().Format(time.RFC1123Z), body)
	if _, err := w.Write([]byte(msg)); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return client.Quit()
}

// headerSafe keeps a header value on one line.
func headerSafe(s string) string {
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(s)
}
