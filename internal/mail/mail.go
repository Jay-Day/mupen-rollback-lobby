// Package mail sends the lobby's account emails (address confirmation and
// password reset codes) over SMTP.
//
// Email is optional. Without RMGK_SMTP_HOST the lobby has no email, the
// public /api/auth/email endpoint answers 404 and RMG-K hides the email
// features; password resets then go through a moderator as before.
package mail

import (
	"bytes"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net"
	netmail "net/mail"
	"net/smtp"
	"os"
	"strconv"
	"strings"
	"time"
)

// Security is how the connection to the SMTP server is protected.
type Security string

const (
	// SecurityStartTLS upgrades a plain connection before logging in (usually port 587).
	SecurityStartTLS Security = "starttls"
	// SecurityTLS uses TLS from the first byte (usually port 465).
	SecurityTLS Security = "tls"
	// SecurityNone sends everything in plain text. Only for a relay on the
	// same machine, such as a local Postfix.
	SecurityNone Security = "none"
)

// Config is the SMTP account the lobby sends from.
type Config struct {
	Host     string
	Port     int
	Username string // empty = no login
	Password string
	From     netmail.Address
	Security Security
}

// Sender delivers one plain-text email.
type Sender interface {
	Send(to, subject, body string) error
}

// ConfigFromEnv reads the RMGK_SMTP_* variables. It returns nil, nil when
// RMGK_SMTP_HOST is unset (email off) and an error when the settings are
// incomplete or invalid.
//
//	RMGK_SMTP_HOST      server name, e.g. smtp.example.com (enables email)
//	RMGK_SMTP_PORT      default 465 for tls, 587 for starttls, 25 for none
//	RMGK_SMTP_SECURITY  starttls (default), tls (default when the port is 465) or none
//	RMGK_SMTP_USERNAME  login, if the server needs one
//	RMGK_SMTP_PASSWORD
//	RMGK_SMTP_FROM      sender, e.g. "RMG-K Lobby <noreply@example.com>"
func ConfigFromEnv() (*Config, error) {
	host := strings.TrimSpace(os.Getenv("RMGK_SMTP_HOST"))
	if host == "" {
		return nil, nil
	}
	cfg := &Config{
		Host:     host,
		Username: os.Getenv("RMGK_SMTP_USERNAME"),
		Password: os.Getenv("RMGK_SMTP_PASSWORD"),
	}

	if raw := strings.TrimSpace(os.Getenv("RMGK_SMTP_PORT")); raw != "" {
		port, err := strconv.Atoi(raw)
		if err != nil || port < 1 || port > 65535 {
			return nil, fmt.Errorf("RMGK_SMTP_PORT %q isn't a port number", raw)
		}
		cfg.Port = port
	}
	switch security := Security(strings.ToLower(strings.TrimSpace(os.Getenv("RMGK_SMTP_SECURITY")))); security {
	case "":
		cfg.Security = SecurityStartTLS
		if cfg.Port == 465 {
			cfg.Security = SecurityTLS
		}
	case SecurityStartTLS, SecurityTLS, SecurityNone:
		cfg.Security = security
	default:
		return nil, fmt.Errorf("RMGK_SMTP_SECURITY %q must be starttls, tls or none", security)
	}
	if cfg.Port == 0 {
		switch cfg.Security {
		case SecurityTLS:
			cfg.Port = 465
		case SecurityNone:
			cfg.Port = 25
		default:
			cfg.Port = 587
		}
	}

	if (cfg.Username == "") != (cfg.Password == "") {
		return nil, errors.New("set both RMGK_SMTP_USERNAME and RMGK_SMTP_PASSWORD, or neither")
	}
	if cfg.Username != "" && cfg.Security == SecurityNone && !isLoopback(cfg.Host) {
		return nil, errors.New("RMGK_SMTP_SECURITY=none would send the SMTP password in plain text; " +
			"use it only with a relay on this machine")
	}

	from := strings.TrimSpace(os.Getenv("RMGK_SMTP_FROM"))
	if from == "" {
		return nil, errors.New("RMGK_SMTP_FROM is required when RMGK_SMTP_HOST is set")
	}
	addr, err := netmail.ParseAddress(from)
	if err != nil {
		return nil, fmt.Errorf("RMGK_SMTP_FROM %q isn't an email address: %w", from, err)
	}
	cfg.From = *addr
	return cfg, nil
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// SMTP sends through the configured server, one connection per email.
type SMTP struct {
	cfg Config
}

// New returns a Sender for cfg.
func New(cfg Config) *SMTP {
	return &SMTP{cfg: cfg}
}

// Send delivers one email. It blocks until the server accepts or refuses it.
func (s *SMTP) Send(to, subject, body string) error {
	if strings.ContainsAny(to, "\r\n") {
		return errors.New("invalid recipient")
	}
	msg, err := buildMessage(s.cfg.From, to, subject, body, time.Now())
	if err != nil {
		return err
	}

	addr := net.JoinHostPort(s.cfg.Host, strconv.Itoa(s.cfg.Port))
	dialer := &net.Dialer{Timeout: 15 * time.Second}
	tlsConfig := &tls.Config{ServerName: s.cfg.Host, MinVersion: tls.VersionTLS12}
	var conn net.Conn
	if s.cfg.Security == SecurityTLS {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, tlsConfig)
	} else {
		conn, err = dialer.Dial("tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("connect to %s: %w", addr, err)
	}
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	client, err := smtp.NewClient(conn, s.cfg.Host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("smtp greeting: %w", err)
	}
	defer client.Close()

	if s.cfg.Security == SecurityStartTLS {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return errors.New("the SMTP server doesn't offer STARTTLS (set RMGK_SMTP_SECURITY)")
		}
		if err := client.StartTLS(tlsConfig); err != nil {
			return fmt.Errorf("starttls: %w", err)
		}
	}
	if s.cfg.Username != "" {
		if err := client.Auth(smtp.PlainAuth("", s.cfg.Username, s.cfg.Password, s.cfg.Host)); err != nil {
			return fmt.Errorf("smtp login: %w", err)
		}
	}
	if err := client.Mail(s.cfg.From.Address); err != nil {
		return fmt.Errorf("smtp sender: %w", err)
	}
	if err := client.Rcpt(to); err != nil {
		return fmt.Errorf("smtp recipient: %w", err)
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("smtp data: %w", err)
	}
	if _, err := w.Write(msg); err != nil {
		return fmt.Errorf("smtp data: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp data: %w", err)
	}
	return client.Quit()
}

// buildMessage formats a plain-text UTF-8 email.
func buildMessage(from netmail.Address, to, subject, body string, now time.Time) ([]byte, error) {
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return nil, err
	}
	domain := "localhost"
	if at := strings.LastIndex(from.Address, "@"); at >= 0 {
		domain = from.Address[at+1:]
	}

	var b bytes.Buffer
	header := func(name, value string) {
		b.WriteString(name + ": " + value + "\r\n")
	}
	header("From", from.String())
	header("To", to)
	header("Subject", mime.QEncoding.Encode("utf-8", subject))
	header("Date", now.Format(time.RFC1123Z))
	header("Message-ID", "<"+hex.EncodeToString(id)+"@"+domain+">")
	header("MIME-Version", "1.0")
	header("Content-Type", "text/plain; charset=utf-8")
	header("Content-Transfer-Encoding", "quoted-printable")
	b.WriteString("\r\n")
	qp := quotedprintable.NewWriter(&b)
	if _, err := qp.Write([]byte(strings.ReplaceAll(body, "\r\n", "\n"))); err != nil {
		return nil, err
	}
	if err := qp.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
