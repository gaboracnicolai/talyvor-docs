// Package email is Talyvor's shared, product-neutral email delivery layer.
// It is reused across products (Track, Docs, …) — nothing in this package
// imports a product's domain types.
//
// Email is strictly opt-in. With EMAIL_ENABLED unset/false the application
// uses a NoopSender and (in practice) never enqueues anything, so behaviour is
// unchanged. Delivery is always asynchronous and best-effort: a slow or down
// SMTP server must never block or fail a core request.
package email

import (
	"context"
	"log/slog"
	"os"
)

// Message is one email to send. To holds one or more recipient addresses.
type Message struct {
	To       []string
	Subject  string
	HTMLBody string
	TextBody string
}

// Sender delivers a single message. Implementations must be safe for
// concurrent use (the queue calls Send from a worker pool).
type Sender interface {
	Send(ctx context.Context, msg Message) error
}

// Config is the SMTP configuration, populated from EMAIL_* env vars. These are
// intentionally unprefixed (not TRACK_*) because the package is shared across
// products.
type Config struct {
	Enabled  bool
	Host     string
	Port     string
	User     string
	Pass     string
	From     string
	FromName string
}

// LoadConfig reads EMAIL_* environment variables.
func LoadConfig() Config {
	return Config{
		Enabled:  parseBool(os.Getenv("EMAIL_ENABLED")),
		Host:     os.Getenv("EMAIL_SMTP_HOST"),
		Port:     getenv("EMAIL_SMTP_PORT", "587"),
		User:     os.Getenv("EMAIL_SMTP_USER"),
		Pass:     os.Getenv("EMAIL_SMTP_PASS"),
		From:     os.Getenv("EMAIL_FROM"),
		FromName: getenv("EMAIL_FROM_NAME", "Talyvor"),
	}
}

// configured reports whether the minimum SMTP settings are present.
func (c Config) configured() bool { return c.Host != "" && c.From != "" }

// NewSender returns an SMTPSender only when email is enabled AND the SMTP
// settings are present; otherwise a NoopSender. This is the single gate that
// keeps email opt-in and prevents a half-configured deployment from producing
// a broken sender.
func NewSender(c Config, logger *slog.Logger) Sender {
	if !c.Enabled || !c.configured() {
		return NewNoopSender(logger)
	}
	return NewSMTPSender(c, logger)
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func parseBool(v string) bool {
	switch v {
	case "1", "true", "TRUE", "True", "yes", "YES", "Yes", "on", "ON", "On":
		return true
	}
	return false
}
