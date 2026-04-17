package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
)

type stubHandlerContext struct {
	ctx            context.Context
	requestHeaders map[string]string
	body           []byte
	bodyErr        error
	response       map[string]string
	statusCode     int
	errMessage     string
	done           chan struct{}
	supportsSSE    bool
	sseEvents      []json.RawMessage
}

func newStubHandlerContext() *stubHandlerContext {
	return &stubHandlerContext{
		ctx:            context.Background(),
		requestHeaders: make(map[string]string),
		response:       make(map[string]string),
		done:           make(chan struct{}),
	}
}

func (c *stubHandlerContext) Context() context.Context           { return c.ctx }
func (c *stubHandlerContext) RequestHeader(key string) string     { return c.requestHeaders[key] }
func (c *stubHandlerContext) RequestBody() ([]byte, error)        { return c.body, c.bodyErr }
func (c *stubHandlerContext) SetResponseHeader(key, value string) { c.response[key] = value }
func (c *stubHandlerContext) WriteError(code int, msg string)     { c.statusCode, c.errMessage = code, msg }
func (c *stubHandlerContext) SetStatusCode(code int)              { c.statusCode = code }
func (c *stubHandlerContext) Flush()                              {}
func (c *stubHandlerContext) Done() <-chan struct{}               { return c.done }
func (c *stubHandlerContext) SupportsSSE() bool                   { return c.supportsSSE }
func (c *stubHandlerContext) WriteSSEEvent(msg json.RawMessage) error {
	c.sseEvents = append(c.sseEvents, append(json.RawMessage(nil), msg...))
	return nil
}
func (c *stubHandlerContext) WriteSSEKeepAlive() error { return nil }
func (c *stubHandlerContext) CloseSSE()                {}

func TestServeProtocolMethodRejectsUnsupportedMethod(t *testing.T) {
	ctx := newStubHandlerContext()
	called := false

	ServeProtocolMethod(ctx, http.MethodPut,
		func() { called = true },
		func() { called = true },
		func() { called = true },
	)

	if called {
		t.Fatal("expected unsupported method to avoid invoking handlers")
	}
	if ctx.statusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", ctx.statusCode, http.StatusMethodNotAllowed)
	}
	if ctx.errMessage != "method not allowed" {
		t.Fatalf("error message = %q, want %q", ctx.errMessage, "method not allowed")
	}
	if got := ctx.response["Allow"]; got != allowMethodsGetPostDelete {
		t.Fatalf("Allow header = %q, want %q", got, allowMethodsGetPostDelete)
	}
}

func TestConnectionCreateErrorMessageHidesInternalServerErrors(t *testing.T) {
	err := errors.New("dial database failed")

	if got := connectionCreateErrorMessage(http.StatusInternalServerError, err); got != "failed to create connection" {
		t.Fatalf("message = %q, want %q", got, "failed to create connection")
	}
	if got := connectionCreateErrorMessage(http.StatusTooManyRequests, err); got != err.Error() {
		t.Fatalf("message = %q, want %q", got, err.Error())
	}
}

func TestPendingRequestsDeliverAndCancel(t *testing.T) {
	pr := NewPendingRequests()
	defer pr.Close()

	ch := pr.Register("n:1")
	if !pr.Deliver("n:1", ReverseCallResponse{Result: json.RawMessage(`{"ok":true}`)}) {
		t.Fatal("expected Deliver to return true")
	}
	resp := <-ch
	if string(resp.Result) != `{"ok":true}` {
		t.Fatalf("result = %s, want {\"ok\":true}", string(resp.Result))
	}

	// Deliver to unknown key returns false.
	if pr.Deliver("n:99", ReverseCallResponse{}) {
		t.Fatal("expected Deliver to return false for unknown key")
	}

	// Cancel closes the channel.
	ch2 := pr.Register("n:2")
	pr.Cancel("n:2")
	if _, ok := <-ch2; ok {
		t.Fatal("expected cancelled channel to be closed")
	}
}
