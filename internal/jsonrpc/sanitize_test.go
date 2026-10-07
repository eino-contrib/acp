package jsonrpc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"testing"
	"time"

	acp "github.com/eino-contrib/acp"
)

// sanitizeCanary is planted in handler errors and panic values; the wire
// JSON must never contain it when sanitization is enabled.
const sanitizeCanary = "wire-canary-4c1f9a"

// startSanitizeConn starts a connection with the given handler over the
// scripted channelTransport and returns the transport for frame exchange.
func startSanitizeConn(t *testing.T, handler MethodHandler, opts ...ConnectionOption) *channelTransport {
	t.Helper()
	transport := newChannelTransport()
	conn := NewConnection(transport, handler, nil, opts...)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- conn.Start(ctx) }()
	if err := conn.WaitUntilStarted(ctx); err != nil {
		cancel()
		t.Fatalf("wait until started: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Errorf("connection did not stop after cancellation")
		}
	})
	return transport
}

// exchangeRequest sends one request frame and returns the raw response frame.
func exchangeRequest(t *testing.T, transport *channelTransport, frame string) json.RawMessage {
	t.Helper()
	transport.inbox <- json.RawMessage(frame)
	select {
	case raw := <-transport.outbox:
		return raw
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for response")
		return nil
	}
}

// errorResponse unmarshals a response frame and returns its error object.
func errorResponse(t *testing.T, raw json.RawMessage) *acp.RPCError {
	t.Helper()
	var msg Message
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if msg.Error == nil {
		t.Fatalf("expected error response, got %s", string(raw))
	}
	return msg.Error
}

func assertSanitizedInternalError(t *testing.T, raw json.RawMessage, absent []string) {
	t.Helper()
	rpcErr := errorResponse(t, raw)
	if rpcErr.Code != int(acp.ErrorCodeInternalError) {
		t.Fatalf("error code = %d, want %d", rpcErr.Code, int(acp.ErrorCodeInternalError))
	}
	if rpcErr.Message != genericInternalErrorMessage {
		t.Fatalf("error message = %q, want %q", rpcErr.Message, genericInternalErrorMessage)
	}
	if len(rpcErr.Data) != 0 {
		t.Fatalf("sanitized error carried data: %s", rpcErr.Data)
	}
	for _, fragment := range absent {
		if bytes.Contains(raw, []byte(fragment)) {
			t.Fatalf("sanitized wire JSON leaks %q: %s", fragment, raw)
		}
	}
}

func TestSanitizedErrorsHideHandlerErrorDetail(t *testing.T) {
	deliberate := acp.NewRPCError(-32002, "deliberate detail", json.RawMessage(`{"origin":"handler"}`))
	handler := func(_ context.Context, method string, _ json.RawMessage) (any, error) {
		if method == "test/deliberate" {
			return nil, deliberate
		}
		return nil, errors.New(sanitizeCanary)
	}
	transport := startSanitizeConn(t, handler, WithSanitizedErrors())

	raw := exchangeRequest(t, transport, `{"jsonrpc":"2.0","id":1,"method":"test/error","params":{}}`)
	assertSanitizedInternalError(t, raw, []string{sanitizeCanary, "originError"})

	// A deliberately returned *acp.RPCError keeps its data even when
	// sanitization is enabled.
	raw = exchangeRequest(t, transport, `{"jsonrpc":"2.0","id":2,"method":"test/deliberate","params":{}}`)
	rpcErr := errorResponse(t, raw)
	if rpcErr.Code != deliberate.Code || rpcErr.Message != deliberate.Message {
		t.Fatalf("deliberate rpc error rewritten: %+v", rpcErr)
	}
	if string(rpcErr.Data) != `{"origin":"handler"}` {
		t.Fatalf("deliberate rpc error data = %s, want %s", rpcErr.Data, `{"origin":"handler"}`)
	}
}

func TestDefaultWireErrorsCarryHandlerErrorDetail(t *testing.T) {
	handler := func(context.Context, string, json.RawMessage) (any, error) {
		return nil, errors.New(sanitizeCanary)
	}
	transport := startSanitizeConn(t, handler)

	raw := exchangeRequest(t, transport, `{"jsonrpc":"2.0","id":1,"method":"test/error","params":{}}`)
	rpcErr := errorResponse(t, raw)
	if rpcErr.Message != genericInternalErrorMessage {
		t.Fatalf("error message = %q, want %q", rpcErr.Message, genericInternalErrorMessage)
	}
	if !bytes.Contains(raw, []byte(sanitizeCanary)) {
		t.Fatalf("default wire JSON dropped handler error detail: %s", raw)
	}
	if !bytes.Contains(raw, []byte("originError")) {
		t.Fatalf("default wire JSON dropped originError data: %s", raw)
	}
}

func TestSanitizedErrorsHidePanicDetail(t *testing.T) {
	handler := func(context.Context, string, json.RawMessage) (any, error) {
		panic("panic-" + sanitizeCanary)
	}
	transport := startSanitizeConn(t, handler, WithSanitizedErrors())

	raw := exchangeRequest(t, transport, `{"jsonrpc":"2.0","id":1,"method":"test/panic","params":{}}`)
	assertSanitizedInternalError(t, raw, []string{sanitizeCanary, "goroutine", ".go:"})
}

func TestDefaultWireErrorsCarryPanicDetail(t *testing.T) {
	handler := func(context.Context, string, json.RawMessage) (any, error) {
		panic("panic-" + sanitizeCanary)
	}
	transport := startSanitizeConn(t, handler)

	raw := exchangeRequest(t, transport, `{"jsonrpc":"2.0","id":1,"method":"test/panic","params":{}}`)
	rpcErr := errorResponse(t, raw)
	if rpcErr.Message != genericInternalErrorMessage {
		t.Fatalf("error message = %q, want %q", rpcErr.Message, genericInternalErrorMessage)
	}
	for _, fragment := range []string{sanitizeCanary, "goroutine", ".go:"} {
		if !bytes.Contains(raw, []byte(fragment)) {
			t.Fatalf("default wire JSON dropped panic detail %q: %s", fragment, raw)
		}
	}
}

// TestMarshalFailureWireErrorsHonorSanitization covers the response-marshal
// failure path: an unmarshalable handler result must not leak the marshal
// error (math.Inf produces "json: unsupported value").
func TestMarshalFailureWireErrorsHonorSanitization(t *testing.T) {
	handler := func(context.Context, string, json.RawMessage) (any, error) {
		return math.Inf(1), nil
	}

	t.Run("sanitized", func(t *testing.T) {
		transport := startSanitizeConn(t, handler, WithSanitizedErrors())
		raw := exchangeRequest(t, transport, `{"jsonrpc":"2.0","id":1,"method":"test/marshal","params":{}}`)
		assertSanitizedInternalError(t, raw, []string{"unsupported value", "originError"})
	})

	t.Run("default", func(t *testing.T) {
		transport := startSanitizeConn(t, handler)
		raw := exchangeRequest(t, transport, `{"jsonrpc":"2.0","id":1,"method":"test/marshal","params":{}}`)
		if !bytes.Contains(raw, []byte("unsupported value")) {
			t.Fatalf("default wire JSON dropped marshal error detail: %s", raw)
		}
		if !bytes.Contains(raw, []byte("originError")) {
			t.Fatalf("default wire JSON dropped originError data: %s", raw)
		}
	})
}
