package conn

import (
	"context"
	"encoding/json"
	"fmt"

	acp "github.com/eino-contrib/acp"
)

type requestDispatcher func(context.Context, json.RawMessage) (any, error)

type notificationDispatcher func(context.Context, json.RawMessage) error

// validatable is implemented by generated request/notification types.
type validatable interface {
	Validate() error
}

func bindRequestHandler[T any, R any](fn func(context.Context, T) (R, error)) requestDispatcher {
	return func(ctx context.Context, params json.RawMessage) (any, error) {
		return unmarshalAndCall[T, R](ctx, params, fn)
	}
}

func bindNotificationHandler[T any](label string, fn func(context.Context, T) error) notificationDispatcher {
	return func(ctx context.Context, params json.RawMessage) error {
		p, err := decodeNotificationParams[T](params)
		if err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
		return fn(ctx, p)
	}
}

// decodeParams unmarshals and validates JSON-RPC params into a typed value.
// The wrapErr function controls how errors are wrapped (e.g. as RPCError for
// requests, or plain errors for notifications).
func decodeParams[T any](params json.RawMessage, wrapErr func(string) error) (T, error) {
	var p T
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return p, wrapErr("invalid params")
		}
	}
	if v, ok := any(&p).(validatable); ok {
		if err := v.Validate(); err != nil {
			return p, wrapErr(err.Error())
		}
	}
	return p, nil
}

func decodeRequestParams[T any](params json.RawMessage) (T, error) {
	return decodeParams[T](params, func(msg string) error {
		return acp.ErrInvalidParams(msg)
	})
}

func decodeNotificationParams[T any](params json.RawMessage) (T, error) {
	return decodeParams[T](params, func(msg string) error {
		return fmt.Errorf("%s", msg)
	})
}

func unmarshalAndCall[T any, R any](ctx context.Context, params json.RawMessage, fn func(context.Context, T) (R, error)) (any, error) {
	p, err := decodeRequestParams[T](params)
	if err != nil {
		return nil, err
	}
	result, err := fn(ctx, p)
	if err != nil {
		return nil, err
	}
	return result, nil
}

func dispatchRequest(ctx context.Context, method string, params json.RawMessage,
	handlers map[string]requestDispatcher, extHandler acp.ExtMethodHandler) (any, error) {
	if acp.IsExtMethod(method) {
		if extHandler != nil {
			return extHandler.HandleExtMethod(ctx, method, params)
		}
		return nil, acp.ErrMethodNotFound(method)
	}

	handler, ok := handlers[method]
	if !ok {
		return nil, acp.ErrMethodNotFound(method)
	}
	return handler(ctx, params)
}

func dispatchNotification(ctx context.Context, method string, params json.RawMessage,
	handlers map[string]notificationDispatcher, extHandler acp.ExtNotificationHandler) error {
	if acp.IsExtMethod(method) {
		if extHandler != nil {
			return extHandler.HandleExtNotification(ctx, method, params)
		}
		return unknownNotificationError(method)
	}

	handler, ok := handlers[method]
	if !ok {
		return unknownNotificationError(method)
	}
	return handler(ctx, params)
}

func unknownNotificationError(method string) error {
	return fmt.Errorf("unsupported notification: %s", method)
}
