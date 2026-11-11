package ws

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jay-day/rmgk-lobby/internal/protocol"
)

type sentMail struct {
	to, subject, body string
}

// fakeMailer records what the hub sends; with fail set every send fails.
type fakeMailer struct {
	sent chan sentMail
	fail error
}

func newFakeMailer() *fakeMailer {
	return &fakeMailer{sent: make(chan sentMail, 16)}
}

func (m *fakeMailer) Send(to, subject, body string) error {
	if m.fail != nil {
		return m.fail
	}
	m.sent <- sentMail{to, subject, body}
	return nil
}

func (m *fakeMailer) next(t *testing.T) sentMail {
	t.Helper()
	select {
	case mail := <-m.sent:
		return mail
	case <-time.After(3 * time.Second):
		t.Fatal("no email was sent")
		return sentMail{}
	}
}

// none checks that nothing was sent. Only use it when the hub returned
// without starting a send.
func (m *fakeMailer) none(t *testing.T) {
	t.Helper()
	select {
	case mail := <-m.sent:
		t.Fatalf("unexpected email to %s: %s", mail.to, mail.subject)
	default:
	}
}

var emailCodePattern = regexp.MustCompile(`[A-Z2-9]{4}-[A-Z2-9]{4}-[A-Z2-9]{4}`)

func codeIn(t *testing.T, mail sentMail) string {
	t.Helper()
	code := emailCodePattern.FindString(mail.body)
	if code == "" {
		t.Fatalf("no code in email:\n%s", mail.body)
	}
	return code
}

// awaitEnvelope waits for a message of msgType (email replies come from a
// goroutine once the mail server has answered).
func awaitEnvelope(t *testing.T, c *Client, msgType string) protocol.Envelope {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case raw := <-c.send:
			var env protocol.Envelope
			if json.Unmarshal(raw, &env) == nil && env.Type == msgType {
				return env
			}
		case <-deadline:
			t.Fatalf("no %s", msgType)
			return protocol.Envelope{}
		}
	}
}

func emailAddress(t *testing.T, env protocol.Envelope) string {
	t.Helper()
	var info protocol.AccountEmailInfo
	if err := json.Unmarshal(env.Data, &info); err != nil {
		t.Fatal(err)
	}
	return info.Address
}

func emailFailReason(t *testing.T, env protocol.Envelope) string {
	t.Helper()
	var fail protocol.AccountPasswordFailPayload
	if err := json.Unmarshal(env.Data, &fail); err != nil {
		t.Fatal(err)
	}
	return fail.Reason
}

func TestEmailOffWithoutSMTP(t *testing.T) {
	f := newRankedFixture(t)
	c := f.connect("ace", true)
	if info := f.hub.accountInfo(c.user); info.Email != nil {
		t.Fatalf("HELLO_OK email without SMTP = %+v", info.Email)
	}
	f.hub.handleAccountSetEmail(c, envelope(t, protocol.AccountSetEmailRequest{Email: "ace@example.com"}))
	if got := awaitEnvelope(t, c, protocol.TypeAccountEmailFail); emailFailReason(t, got) != protocol.ReasonEmailUnavailable {
		t.Fatalf("set email without SMTP = %s", got.Data)
	}
	if wait := f.hub.RequestPasswordResetEmail("ace", "ace@example.com", "203.0.113.5"); wait != 0 {
		t.Fatalf("reset request without SMTP waits %v", wait)
	}
	reset := f.hub.authenticateHello(c, &protocol.HelloRequest{Username: "ace",
		Auth: &protocol.HelloAuth{Mode: "reset", ResetCode: "AAAA-AAAA-AAAA", Password: "newpassword1"}})
	if strings.Contains(reset.message, "Email") {
		t.Fatalf("bad code message offers email without SMTP: %q", reset.message)
	}
}

func TestConfirmEmailThenResetByEmail(t *testing.T) {
	f := newRankedFixture(t)
	mailer := newFakeMailer()
	f.hub.EnableEmail(mailer)
	c := f.connect("ace", true)
	c.ip = "203.0.113.5"
	guest := f.connect("guest", false)

	if info := f.hub.accountInfo(c.user); info.Email == nil || info.Email.Address != "" {
		t.Fatalf("HELLO_OK email before adding one = %+v", info.Email)
	}
	f.hub.handleAccountSetEmail(guest, envelope(t, protocol.AccountSetEmailRequest{Email: "g@example.com"}))
	if got := awaitEnvelope(t, guest, protocol.TypeAccountEmailFail); emailFailReason(t, got) != protocol.ReasonNotAuthorized {
		t.Fatalf("guest set email = %s", got.Data)
	}
	f.hub.handleAccountSetEmail(c, envelope(t, protocol.AccountSetEmailRequest{Email: "not an address"}))
	if got := awaitEnvelope(t, c, protocol.TypeAccountEmailFail); emailFailReason(t, got) != protocol.ReasonInvalidEmail {
		t.Fatalf("bad address = %s", got.Data)
	}

	f.hub.handleAccountSetEmail(c, envelope(t, protocol.AccountSetEmailRequest{Email: "Ace@Example.com"}))
	if got := emailAddress(t, awaitEnvelope(t, c, protocol.TypeAccountEmailCodeSent)); got != "Ace@example.com" {
		t.Fatalf("code sent to %q", got)
	}
	confirm := mailer.next(t)
	if confirm.to != "Ace@example.com" || !strings.Contains(confirm.body, "Hi ace") {
		t.Fatalf("confirmation email = %+v", confirm)
	}
	f.hub.handleAccountConfirmEmail(c, envelope(t, protocol.AccountConfirmEmailRequest{Code: "AAAA-AAAA-AAAA"}))
	if got := awaitEnvelope(t, c, protocol.TypeAccountEmailFail); emailFailReason(t, got) != protocol.ReasonAuthFailed {
		t.Fatalf("wrong confirmation code = %s", got.Data)
	}
	f.hub.handleAccountConfirmEmail(c, envelope(t, protocol.AccountConfirmEmailRequest{Code: codeIn(t, confirm)}))
	if got := emailAddress(t, awaitEnvelope(t, c, protocol.TypeAccountEmail)); got != "Ace@example.com" {
		t.Fatalf("confirmed address = %q", got)
	}
	if info := f.hub.accountInfo(c.user); info.Email == nil || info.Email.Address != "Ace@example.com" {
		t.Fatalf("HELLO_OK email after confirming = %+v", info.Email)
	}

	// unknown names, malformed ones and someone else's address send nothing,
	// and get the same answer
	if wait := f.hub.RequestPasswordResetEmail("ghost", "ghost@example.com", "198.51.100.1"); wait != 0 {
		t.Fatalf("unknown account wait = %v", wait)
	}
	f.hub.RequestPasswordResetEmail("bad name!", "ace@example.com", "198.51.100.1")
	if wait := f.hub.RequestPasswordResetEmail("ace", "attacker@example.com", "198.51.100.1"); wait != 0 {
		t.Fatalf("wrong address wait = %v", wait)
	}
	f.hub.RequestPasswordResetEmail("ace", "", "198.51.100.1")
	mailer.none(t)

	f.hub.RequestPasswordResetEmail("ACE", "ace@EXAMPLE.com", "198.51.100.5")
	reset := mailer.next(t)
	if reset.to != "Ace@example.com" || !strings.Contains(reset.subject, "reset") {
		t.Fatalf("reset email = %+v", reset)
	}
	result := f.hub.authenticateHello(c, &protocol.HelloRequest{Username: "ace",
		Auth: &protocol.HelloAuth{Mode: "reset", ResetCode: codeIn(t, reset), Password: "emailedpass1"}})
	if result.account == nil || result.token == "" {
		t.Fatalf("reset with the emailed code = %+v", result)
	}
	if _, err := f.store.Login("ace", "emailedpass1"); err != nil {
		t.Fatalf("login after the emailed reset: %v", err)
	}
	bad := f.hub.authenticateHello(c, &protocol.HelloRequest{Username: "ace",
		Auth: &protocol.HelloAuth{Mode: "reset", ResetCode: "AAAA-AAAA-AAAA", Password: "emailedpass2"}})
	if !strings.Contains(bad.message, "Email yourself a new one") {
		t.Fatalf("bad code message with SMTP = %q", bad.message)
	}

	// three reset emails per account per hour, from any address
	f.hub.RequestPasswordResetEmail("ace", "Ace@example.com", "198.51.100.2")
	f.hub.RequestPasswordResetEmail("ace", "Ace@example.com", "198.51.100.3")
	mailer.next(t)
	mailer.next(t)
	f.hub.RequestPasswordResetEmail("ace", "Ace@example.com", "198.51.100.4")
	mailer.none(t)

	// five requests per IP per 15 minutes, then a wait
	for i := 0; i < resetRequestLimit; i++ {
		if wait := f.hub.RequestPasswordResetEmail("ghost", "ghost@example.com", "192.0.2.9"); wait != 0 {
			t.Fatalf("request %d waits %v", i+1, wait)
		}
	}
	if wait := f.hub.RequestPasswordResetEmail("ghost", "ghost@example.com", "192.0.2.9"); wait <= 0 {
		t.Fatal("no wait after too many requests from one IP")
	}

	f.hub.handleAccountRemoveEmail(c, envelope(t, struct{}{}))
	if got := emailAddress(t, awaitEnvelope(t, c, protocol.TypeAccountEmail)); got != "" {
		t.Fatalf("address after removing = %q", got)
	}
}

func TestEmailSendFailureIsReported(t *testing.T) {
	f := newRankedFixture(t)
	mailer := newFakeMailer()
	mailer.fail = errors.New("connection refused")
	f.hub.EnableEmail(mailer)
	c := f.connect("ace", true)

	f.hub.handleAccountSetEmail(c, envelope(t, protocol.AccountSetEmailRequest{Email: "ace@example.com"}))
	if got := awaitEnvelope(t, c, protocol.TypeAccountEmailFail); emailFailReason(t, got) != protocol.ReasonEmailFailed {
		t.Fatalf("failed send = %s", got.Data)
	}
}
