package core

import "context"

type outboxUUIDKey struct{}

// WithOutboxUUID attaches an outbox idempotency UUID to ctx. Platforms read it
// while sending a final reply and pass it as the platform's dedup key, so the
// immediate attempt and every replay of one reply collapse to a single message
// even if an in-process race slips through. An empty uuid returns ctx
// unchanged so every non-outbox send behaves exactly as before.
func WithOutboxUUID(ctx context.Context, uuid string) context.Context {
	if uuid == "" {
		return ctx
	}
	return context.WithValue(ctx, outboxUUIDKey{}, uuid)
}

// OutboxUUIDFromContext extracts the idempotency UUID attached by
// WithOutboxUUID. It returns "" when the send is not outbox-backed.
func OutboxUUIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(outboxUUIDKey{}).(string); ok {
		return v
	}
	return ""
}
