package email

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/smtp"
	"strings"
)

// transportFunc abstracts the actual SMTP send so tests can substitute a fake.
// The default implementation is net/smtp.SendMail, which performs STARTTLS
// automatically when the server advertises it.
type transportFunc func(addr string, auth smtp.Auth, from string, to []string, msg []byte) error

// SMTPSender sends via an SMTP relay (Resend/Postmark/SES/any) using
// net/smtp + STARTTLS.
type SMTPSender struct {
	host     string
	port     string
	user     string
	pass     string
	from     string
	fromName string
	logger   *slog.Logger
	send     transportFunc
}

func NewSMTPSender(c Config, logger *slog.Logger) *SMTPSender {
	if logger == nil {
		logger = slog.Default()
	}
	return &SMTPSender{
		host: c.Host, port: c.Port, user: c.User, pass: c.Pass,
		from: c.From, fromName: c.FromName, logger: logger,
		send: smtp.SendMail,
	}
}

// Send builds the MIME message and hands it to the transport. Returns an error
// (so the queue can retry) but is a no-op with no recipients.
func (s *SMTPSender) Send(_ context.Context, msg Message) error {
	if len(msg.To) == 0 {
		return nil
	}
	var auth smtp.Auth
	if s.user != "" {
		auth = smtp.PlainAuth("", s.user, s.pass, s.host)
	}
	addr := s.host + ":" + s.port
	if err := s.send(addr, auth, s.from, msg.To, s.buildMIME(msg)); err != nil {
		return fmt.Errorf("email: smtp send: %w", err)
	}
	return nil
}

// buildMIME renders an RFC-822 multipart/alternative message carrying both the
// plain-text and HTML bodies. Email clients pick the richest part they support.
func (s *SMTPSender) buildMIME(msg Message) []byte {
	const boundary = "talyvor-boundary-7f3a9c2e1b"
	var b bytes.Buffer

	from := sanitizeHeader(s.from)
	if s.fromName != "" {
		from = fmt.Sprintf("%s <%s>", sanitizeHeader(s.fromName), sanitizeHeader(s.from))
	}
	to := make([]string, 0, len(msg.To))
	for _, addr := range msg.To {
		to = append(to, sanitizeHeader(addr))
	}
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(to, ", "))
	fmt.Fprintf(&b, "Subject: %s\r\n", sanitizeHeader(msg.Subject))
	b.WriteString("MIME-Version: 1.0\r\n")
	fmt.Fprintf(&b, "Content-Type: multipart/alternative; boundary=\"%s\"\r\n", boundary)
	b.WriteString("\r\n")

	// Plain-text part first (lowest fidelity), per RFC 2046 ordering.
	fmt.Fprintf(&b, "--%s\r\n", boundary)
	b.WriteString("Content-Type: text/plain; charset=\"utf-8\"\r\n\r\n")
	b.WriteString(msg.TextBody)
	b.WriteString("\r\n")

	fmt.Fprintf(&b, "--%s\r\n", boundary)
	b.WriteString("Content-Type: text/html; charset=\"utf-8\"\r\n\r\n")
	b.WriteString(msg.HTMLBody)
	b.WriteString("\r\n")

	fmt.Fprintf(&b, "--%s--\r\n", boundary)
	return b.Bytes()
}

// sanitizeHeader neutralizes SMTP header (CRLF) injection. Header values are
// derived from user-controlled data (page titles, …), so any CR or LF — the
// only bytes that can start a new header line — is replaced with a space
// before the value is written into a header.
func sanitizeHeader(v string) string {
	if !strings.ContainsAny(v, "\r\n") {
		return v
	}
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(v)
}
