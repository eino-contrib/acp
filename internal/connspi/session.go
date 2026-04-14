package connspi

import "context"

type sessionIDKey struct{}

func WithSessionID(ctx context.Context, sessionID string) context.Context {
	if sessionID == "" {
		return ctx
	}
	return context.WithValue(ctx, sessionIDKey{}, sessionID)
}

func SessionIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(sessionIDKey{}).(string); ok {
		return v
	}
	return ""
}

// SessionIDProvider is implemented by generated types that carry a SessionID
// field. The code generator emits GetSessionID on every such type, so
// ExtractSessionIDFromAny only needs the interface path.
type SessionIDProvider interface {
	GetSessionID() string
}

func ExtractSessionIDFromAny(v any) string {
	if p, ok := v.(SessionIDProvider); ok {
		return p.GetSessionID()
	}
	return ""
}
