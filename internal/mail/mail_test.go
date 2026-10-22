package mail

import (
	"bufio"
	"io"
	"mime/quotedprintable"
	"net"
	netmail "net/mail"
	"strings"
	"testing"
)

func setSMTPEnv(t *testing.T, vars map[string]string) {
	t.Helper()
	for _, key := range []string{"RMGK_SMTP_HOST", "RMGK_SMTP_PORT", "RMGK_SMTP_SECURITY",
		"RMGK_SMTP_USERNAME", "RMGK_SMTP_PASSWORD", "RMGK_SMTP_FROM"} {
		t.Setenv(key, vars[key])
	}
}

func TestConfigFromEnv(t *testing.T) {
	setSMTPEnv(t, nil)
	if cfg, err := ConfigFromEnv(); cfg != nil || err != nil {
		t.Fatalf("unset = %+v, %v; want email off", cfg, err)
	}

	setSMTPEnv(t, map[string]string{"RMGK_SMTP_HOST": "smtp.example.com"})
	if _, err := ConfigFromEnv(); err == nil {
		t.Fatal("a host without RMGK_SMTP_FROM should be an error")
	}

	setSMTPEnv(t, map[string]string{
		"RMGK_SMTP_HOST": "smtp.example.com", "RMGK_SMTP_FROM": "RMG-K Lobby <noreply@example.com>",
		"RMGK_SMTP_USERNAME": "lobby",
	})
	if _, err := ConfigFromEnv(); err == nil {
		t.Fatal("a username without a password should be an error")
	}

	setSMTPEnv(t, map[string]string{
		"RMGK_SMTP_HOST": "smtp.example.com", "RMGK_SMTP_FROM": "RMG-K Lobby <noreply@example.com>",
		"RMGK_SMTP_USERNAME": "lobby", "RMGK_SMTP_PASSWORD": "secret",
	})
	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != 587 || cfg.Security != SecurityStartTLS || cfg.From.Address != "noreply@example.com" ||
		cfg.From.Name != "RMG-K Lobby" {
		t.Fatalf("defaults = %+v", cfg)
	}

	setSMTPEnv(t, map[string]string{"RMGK_SMTP_HOST": "smtp.example.com", "RMGK_SMTP_PORT": "465",
		"RMGK_SMTP_FROM": "noreply@example.com"})
	if cfg, err = ConfigFromEnv(); err != nil || cfg.Security != SecurityTLS {
		t.Fatalf("port 465 = %+v, %v; want implicit TLS", cfg, err)
	}

	setSMTPEnv(t, map[string]string{"RMGK_SMTP_HOST": "smtp.example.com", "RMGK_SMTP_SECURITY": "none",
		"RMGK_SMTP_USERNAME": "lobby", "RMGK_SMTP_PASSWORD": "secret", "RMGK_SMTP_FROM": "noreply@example.com"})
	if _, err := ConfigFromEnv(); err == nil {
		t.Fatal("a plain-text login to a remote server should be refused")
	}

	setSMTPEnv(t, map[string]string{"RMGK_SMTP_HOST": "127.0.0.1", "RMGK_SMTP_SECURITY": "none",
		"RMGK_SMTP_FROM": "noreply@example.com"})
	if cfg, err = ConfigFromEnv(); err != nil || cfg.Port != 25 {
		t.Fatalf("local relay = %+v, %v", cfg, err)
	}

	for _, bad := range []map[string]string{
		{"RMGK_SMTP_HOST": "smtp.example.com", "RMGK_SMTP_FROM": "noreply@example.com", "RMGK_SMTP_PORT": "x"},
		{"RMGK_SMTP_HOST": "smtp.example.com", "RMGK_SMTP_FROM": "noreply@example.com", "RMGK_SMTP_SECURITY": "ssl"},
		{"RMGK_SMTP_HOST": "smtp.example.com", "RMGK_SMTP_FROM": "not an address"},
	} {
		setSMTPEnv(t, bad)
		if _, err := ConfigFromEnv(); err == nil {
			t.Fatalf("%v should be an error", bad)
		}
	}
}

// fakeSMTP accepts messages on a loopback port and returns them on a channel.
func fakeSMTP(t *testing.T) (port int, got <-chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	out := make(chan string, 4)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serveFakeSMTP(conn, out)
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port, out
}

func serveFakeSMTP(conn net.Conn, out chan<- string) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	say := func(line string) { io.WriteString(conn, line+"\r\n") }
	say("220 fake")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		switch cmd := strings.ToUpper(strings.Fields(line)[0]); cmd {
		case "EHLO", "HELO":
			say("250 fake")
		case "DATA":
			say("354 go ahead")
			var msg strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil || l == ".\r\n" {
					break
				}
				msg.WriteString(strings.TrimPrefix(l, "."))
			}
			out <- msg.String()
			say("250 queued")
		case "QUIT":
			say("221 bye")
			return
		default:
			say("250 ok")
		}
	}
}

func TestSendPlainTextEmail(t *testing.T) {
	port, got := fakeSMTP(t)
	sender := New(Config{Host: "127.0.0.1", Port: port, Security: SecurityNone,
		From: netmail.Address{Name: "RMG-K Lobby", Address: "noreply@example.com"}})
	body := "Hi Ace,\n\nYour code: ABCD-EFGH-JKMN\n\nIt works once — then it's gone."
	if err := sender.Send("ace@example.com", "Your RMG-K password reset code", body); err != nil {
		t.Fatalf("Send: %v", err)
	}
	raw := <-got
	msg, err := netmail.ReadMessage(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("parse sent message: %v\n%s", err, raw)
	}
	if msg.Header.Get("To") != "ace@example.com" || msg.Header.Get("Subject") != "Your RMG-K password reset code" ||
		!strings.Contains(msg.Header.Get("From"), "noreply@example.com") || msg.Header.Get("Message-Id") == "" {
		t.Fatalf("headers = %v", msg.Header)
	}
	decoded, err := io.ReadAll(quotedprintable.NewReader(msg.Body))
	if err != nil {
		t.Fatal(err)
	}
	// SMTP's end-of-data marker adds the final line break
	if want := strings.ReplaceAll(body, "\n", "\r\n"); strings.TrimSuffix(string(decoded), "\r\n") != want {
		t.Fatalf("body = %q, want %q", decoded, want)
	}

	if err := sender.Send("ace@example.com\r\nBcc: x@example.com", "s", "b"); err == nil {
		t.Fatal("a recipient with a line break should be refused")
	}
	if err := New(Config{Host: "127.0.0.1", Port: port, Security: SecurityStartTLS,
		From: netmail.Address{Address: "noreply@example.com"}}).Send("a@example.com", "s", "b"); err == nil {
		t.Fatal("STARTTLS should be required when it's the configured security")
	}
}
