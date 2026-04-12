package mcp

import (
	"context"
	"strings"
)

type sessionContextKey struct{}

// WithSessionKey binds a stable session key (for example "tg:chat:<id>") to context.
func WithSessionKey(ctx context.Context, key string) context.Context {
	key = strings.TrimSpace(key)
	if key == "" {
		return ctx
	}
	return context.WithValue(ctx, sessionContextKey{}, key)
}

// SessionKeyFromContext returns bound session key or "default".
func SessionKeyFromContext(ctx context.Context) string {
	if ctx == nil {
		return "default"
	}
	if v := ctx.Value(sessionContextKey{}); v != nil {
		if s, ok := v.(string); ok {
			s = strings.TrimSpace(s)
			if s != "" {
				return s
			}
		}
	}
	return "default"
}
