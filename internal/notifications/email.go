package notifications

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"strings"
	"time"
)

// SMTP email channel — FR-NOTIF-012 | MDS §4.17.
//
// The comment on NotificationTask.Channel has read "whatsapp | sms | email"
// since v2 and notification_log's CHECK has allowed 'email' since migration
// 008, but Dispatcher's switch had no email case: an email notification
// could be logged and never sent. This is the missing half.

// ErrEmailNotConfigured is returned when SMTP settings are absent. Callers
// treat it as "this channel is unavailable", not as a delivery failure —
// the same graceful degradation the API's PDF renderer and Razorpay already get.
var ErrEmailNotConfigured = errors.New("notifications: SMTP is not configured")

// EmailSender is the interface the dispatcher depends on, so a test can
// substitute a recorder without opening a socket.
type EmailSender interface {
	SendEmail(ctx context.Context, to, subject, body string) error
}

// SMTPConfig carries the connection settings.
type SMTPConfig struct {
	Host     string
	Port     int
	Username string
	Password string
	From     string
}

// SMTPClient implements EmailSender against a plain SMTP server.
type SMTPClient struct {
	cfg SMTPConfig
	// dial is injectable so tests can exercise the send path against a
	// local listener rather than reaching the network.
	dial func(ctx context.Context, addr string) (*smtp.Client, error)
}

// NewSMTPClient constructs an SMTPClient. A zero-value Host means the
// channel is unconfigured, which SendEmail reports rather than panicking.
func NewSMTPClient(cfg SMTPConfig) *SMTPClient {
	if cfg.Port == 0 {
		cfg.Port = 587
	}
	return &SMTPClient{cfg: cfg, dial: defaultSMTPDial}
}

// defaultSMTPDial honours the caller's context as well as its own timeout,
// so a cancelled notification task stops dialling instead of holding a
// worker for the full ten seconds.
func defaultSMTPDial(ctx context.Context, addr string) (*smtp.Client, error) {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	host, _, _ := net.SplitHostPort(addr) //nolint:errcheck // addr was built from configured host/port
	return smtp.NewClient(conn, host)
}

// Configured reports whether this client can actually send.
func (c *SMTPClient) Configured() bool { return c != nil && c.cfg.Host != "" }

// SendEmail delivers one message.
//
// Auth is attempted only when a username is set: many self-hosted relays on
// a private network accept unauthenticated submission, and demanding
// credentials would make those deployments unserviceable.
func (c *SMTPClient) SendEmail(ctx context.Context, to, subject, body string) error {
	if !c.Configured() {
		return ErrEmailNotConfigured
	}
	if !strings.Contains(to, "@") {
		return fmt.Errorf("notifications: %q is not a valid email address", to)
	}

	addr := fmt.Sprintf("%s:%d", c.cfg.Host, c.cfg.Port)
	client, err := c.dial(ctx, addr)
	if err != nil {
		return fmt.Errorf("notifications: smtp dial %s: %w", addr, err)
	}
	defer client.Close() //nolint:errcheck // best-effort; the send result is what matters

	// STARTTLS where the server offers it. Not mandatory, because a relay on
	// a private network may legitimately not offer it — but never skipped
	// when available, so credentials are not sent in clear over a link that
	// could have been encrypted.
	if ok, _ := client.Extension("STARTTLS"); ok {
		if err := client.StartTLS(&tls.Config{ServerName: c.cfg.Host, MinVersion: tls.VersionTLS12}); err != nil {
			return fmt.Errorf("notifications: smtp starttls: %w", err)
		}
	}

	if c.cfg.Username != "" {
		auth := smtp.PlainAuth("", c.cfg.Username, c.cfg.Password, c.cfg.Host)
		if err := client.Auth(auth); err != nil {
			return fmt.Errorf("notifications: smtp auth: %w", err)
		}
	}

	if err := client.Mail(c.cfg.From); err != nil {
		return fmt.Errorf("notifications: smtp MAIL FROM: %w", err)
	}
	if err := client.Rcpt(to); err != nil {
		return fmt.Errorf("notifications: smtp RCPT TO: %w", err)
	}

	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("notifications: smtp DATA: %w", err)
	}
	msg := buildMessage(c.cfg.From, to, subject, body)
	if _, err := w.Write([]byte(msg)); err != nil {
		return fmt.Errorf("notifications: smtp write body: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("notifications: smtp close body: %w", err)
	}
	return client.Quit()
}

// buildMessage assembles RFC 5322 headers and body.
//
// Newlines in the subject are stripped rather than escaped: a subject
// carrying CRLF could inject arbitrary extra headers (a Bcc, say) into the
// message, and no legitimate subject needs one.
func buildMessage(from, to, subject, body string) string {
	subject = strings.NewReplacer("\r", " ", "\n", " ").Replace(subject)
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", to)
	fmt.Fprintf(&b, "Subject: %s\r\n", subject)
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	b.WriteString("\r\n")
	b.WriteString(body)
	return b.String()
}
