package email

import (
	"context"
	"testing"
)

func TestNewSender_DisabledReturnsNoop(t *testing.T) {
	s := NewSender(Config{Enabled: false, Host: "smtp.example.com", From: "x@y.z"}, nil)
	if _, ok := s.(*NoopSender); !ok {
		t.Fatalf("disabled config should yield *NoopSender, got %T", s)
	}
}

func TestNewSender_EnabledButUnconfiguredReturnsNoop(t *testing.T) {
	// Enabled but missing SMTP host/from — must not produce a broken SMTP
	// sender; fall back to Noop so Track keeps running.
	s := NewSender(Config{Enabled: true}, nil)
	if _, ok := s.(*NoopSender); !ok {
		t.Fatalf("enabled-but-unconfigured should yield *NoopSender, got %T", s)
	}
}

func TestNewSender_EnabledAndConfiguredReturnsSMTP(t *testing.T) {
	s := NewSender(Config{Enabled: true, Host: "smtp.example.com", Port: "587", From: "noreply@x.z"}, nil)
	if _, ok := s.(*SMTPSender); !ok {
		t.Fatalf("enabled+configured should yield *SMTPSender, got %T", s)
	}
}

func TestNoopSender_SendNeverErrors(t *testing.T) {
	s := NewNoopSender(nil)
	err := s.Send(context.Background(), Message{To: []string{"a@b.c"}, Subject: "hi"})
	if err != nil {
		t.Fatalf("NoopSender.Send should never error, got %v", err)
	}
}
