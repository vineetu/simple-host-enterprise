package handler

import (
	"bufio"
	"context"
	"net"
	"strings"
	"testing"
)

// fakeSMTP is a one-connection SMTP server that offers no STARTTLS and
// records the envelope sender and the message.
func fakeSMTP(t *testing.T) (addr string, got chan [2]string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	got = make(chan [2]string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		say := func(s string) { _, _ = conn.Write([]byte(s + "\r\n")) }
		say("220 fake")
		var from, data string
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				got <- [2]string{from, data}
				return
			}
			cmd := strings.ToUpper(strings.TrimSpace(line))
			switch {
			case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
				say("250 fake")
			case strings.HasPrefix(cmd, "MAIL FROM:"):
				from = strings.TrimSpace(line[len("MAIL FROM:"):])
				say("250 ok")
			case strings.HasPrefix(cmd, "RCPT"):
				say("250 ok")
			case cmd == "DATA":
				say("354 go")
				var b strings.Builder
				for {
					l, err := r.ReadString('\n')
					if err != nil || l == ".\r\n" {
						break
					}
					b.WriteString(l)
				}
				data = b.String()
				say("250 queued")
			case cmd == "QUIT":
				say("221 bye")
				got <- [2]string{from, data}
				return
			default:
				say("250 ok")
			}
		}
	}()
	return ln.Addr().String(), got
}

// Without STARTTLS on offer, smtp:// refuses to send in the clear unless
// SMTP_URL says ?insecure=1. The envelope sender is SMTP_FROM's bare
// address, the From header keeps its display name, and a non-ASCII subject
// is RFC 2047 encoded.
func TestSMTPMailer(t *testing.T) {
	addr, _ := fakeSMTP(t)
	m, err := NewSMTPMailer("smtp://"+addr, "Simple Host <hosting@corp.test>")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Send(context.Background(), []string{"a@corp.test"}, "hi", "body"); err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("cleartext send without insecure=1: %v", err)
	}

	addr, got := fakeSMTP(t)
	m, err = NewSMTPMailer("smtp://"+addr+"?insecure=1", "Simple Host <hosting@corp.test>")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Send(context.Background(), []string{"a@corp.test"}, "Café moves on 1 May", "body"); err != nil {
		t.Fatal(err)
	}
	result := <-got
	if result[0] != "<hosting@corp.test>" {
		t.Errorf("MAIL FROM %q, want the bare address", result[0])
	}
	if !strings.Contains(result[1], "From: \"Simple Host\" <hosting@corp.test>\r\n") {
		t.Errorf("From header missing the display name:\n%s", result[1])
	}
	if !strings.Contains(result[1], "Subject: =?utf-8?q?Caf=C3=A9_moves_on_1_May?=\r\n") {
		t.Errorf("subject not encoded:\n%s", result[1])
	}
}
