package email

import (
	"context"
	"net/smtp"
	"strings"
	"testing"
)

func TestBuildMIME_ContainsHeadersAndBothBodies(t *testing.T) {
	s := &SMTPSender{from: "noreply@track.example", fromName: "Talyvor Track"}
	msg := Message{
		To:       []string{"alice@example.com", "bob@example.com"},
		Subject:  "ENG-42 assigned to you",
		HTMLBody: "<p>You were assigned <b>ENG-42</b></p>",
		TextBody: "You were assigned ENG-42",
	}
	raw := string(s.buildMIME(msg))

	for _, want := range []string{
		"From: Talyvor Track <noreply@track.example>",
		"To: alice@example.com, bob@example.com",
		"Subject: ENG-42 assigned to you",
		"MIME-Version: 1.0",
		"multipart/alternative",
		"text/plain",
		"text/html",
		"You were assigned ENG-42",
		"<b>ENG-42</b>",
	} {
		if !strings.Contains(raw, want) {
			t.Errorf("MIME missing %q\n---\n%s", want, raw)
		}
	}
}

func TestSMTPSender_SendInvokesTransportWithRecipientsAndMIME(t *testing.T) {
	var gotAddr, gotFrom string
	var gotTo []string
	var gotMsg []byte
	s := &SMTPSender{
		host: "smtp.example.com", port: "587",
		from: "noreply@track.example", fromName: "Track",
		send: func(addr string, _ smtp.Auth, from string, to []string, msg []byte) error {
			gotAddr, gotFrom, gotTo, gotMsg = addr, from, to, msg
			return nil
		},
	}
	err := s.Send(context.Background(), Message{
		To: []string{"alice@example.com"}, Subject: "Hi", TextBody: "body", HTMLBody: "<p>body</p>",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if gotAddr != "smtp.example.com:587" {
		t.Errorf("addr = %q, want smtp.example.com:587", gotAddr)
	}
	if gotFrom != "noreply@track.example" {
		t.Errorf("from = %q", gotFrom)
	}
	if len(gotTo) != 1 || gotTo[0] != "alice@example.com" {
		t.Errorf("to = %v", gotTo)
	}
	if !strings.Contains(string(gotMsg), "Subject: Hi") {
		t.Errorf("msg missing subject header: %s", gotMsg)
	}
}

// headerLines returns the CRLF-split lines of the header block (before the
// first blank line).
func headerLines(raw string) []string {
	block := raw
	if i := strings.Index(raw, "\r\n\r\n"); i >= 0 {
		block = raw[:i]
	}
	return strings.Split(block, "\r\n")
}

// TestBuildMIME_StripsHeaderInjectionFromSubject is an adversarial-sweep test:
// the Subject is built from user-controlled data (page titles), so a CRLF in
// that data must not inject extra SMTP headers (Bcc, fake bodies, …).
func TestBuildMIME_StripsHeaderInjectionFromSubject(t *testing.T) {
	s := &SMTPSender{from: "noreply@docs.example", fromName: "Docs"}
	msg := Message{
		To:       []string{"alice@example.com"},
		Subject:  "Review requested: Spec\r\nBcc: attacker@evil.example\r\nX-Injected: yes",
		TextBody: "body", HTMLBody: "<p>body</p>",
	}
	raw := string(s.buildMIME(msg))
	for _, line := range headerLines(raw) {
		if strings.HasPrefix(line, "Bcc:") || strings.HasPrefix(line, "X-Injected") {
			t.Errorf("Subject header injection not neutralized — smuggled header line %q present\n%s", line, raw)
		}
	}
	if !strings.Contains(raw, "Subject: Review requested: Spec") {
		t.Errorf("sanitized subject text should still be present:\n%s", raw)
	}
}

// TestBuildMIME_StripsHeaderInjectionFromRecipient is defense-in-depth even
// though addresses are resolved server-side.
func TestBuildMIME_StripsHeaderInjectionFromRecipient(t *testing.T) {
	s := &SMTPSender{from: "noreply@docs.example", fromName: "Docs"}
	msg := Message{
		To:       []string{"alice@example.com\r\nBcc: attacker@evil.example"},
		Subject:  "hi",
		TextBody: "body", HTMLBody: "<p>body</p>",
	}
	raw := string(s.buildMIME(msg))
	for _, line := range headerLines(raw) {
		if strings.HasPrefix(line, "Bcc:") {
			t.Errorf("recipient header injection not neutralized — %q present\n%s", line, raw)
		}
	}
}

func TestSMTPSender_SendNoRecipientsIsNoop(t *testing.T) {
	called := false
	s := &SMTPSender{host: "h", port: "25", from: "f@x.z",
		send: func(string, smtp.Auth, string, []string, []byte) error { called = true; return nil }}
	if err := s.Send(context.Background(), Message{Subject: "x"}); err != nil {
		t.Fatalf("Send with no recipients should be a no-op nil, got %v", err)
	}
	if called {
		t.Error("transport should not be invoked when there are no recipients")
	}
}
