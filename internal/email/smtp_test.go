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
