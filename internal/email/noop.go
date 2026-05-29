package email

import (
	"context"
	"log/slog"
)

// NoopSender logs instead of sending. It is the default whenever email is
// disabled or SMTP is not configured, and serves as a safe dry-run sender.
type NoopSender struct {
	logger *slog.Logger
}

func NewNoopSender(logger *slog.Logger) *NoopSender {
	if logger == nil {
		logger = slog.Default()
	}
	return &NoopSender{logger: logger}
}

func (n *NoopSender) Send(_ context.Context, msg Message) error {
	n.logger.Info("email: noop send (delivery disabled)",
		slog.Any("to", msg.To),
		slog.String("subject", msg.Subject),
	)
	return nil
}
