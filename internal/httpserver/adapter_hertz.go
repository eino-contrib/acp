package httpserver

import (
	"context"
	"encoding/json"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/sse"
)

// hertzContext adapts a Hertz request context to HandlerContext.
type hertzContext struct {
	ctx    context.Context
	c      *app.RequestContext
	writer *sse.Writer
}

// NewHertzHandlerContext creates a HandlerContext for a Hertz request.
// Exported so that server/ and other packages can create HandlerContext
// instances for Hertz request contexts.
func NewHertzHandlerContext(ctx context.Context, c *app.RequestContext) *hertzContext {
	return &hertzContext{ctx: ctx, c: c}
}

func (h *hertzContext) Context() context.Context {
	if h != nil && h.ctx != nil {
		return h.ctx
	}
	return context.Background()
}

func (h *hertzContext) RequestHeader(key string) string {
	return string(h.c.GetHeader(key))
}

func (h *hertzContext) RequestBody() ([]byte, error) {
	bodyBytes, err := h.c.Body()
	if err != nil {
		return nil, err
	}
	body := make([]byte, len(bodyBytes))
	copy(body, bodyBytes)
	return body, nil
}

func (h *hertzContext) SetResponseHeader(key, value string) {
	h.c.Response.Header.Set(key, value)
}

func (h *hertzContext) WriteError(code int, msg string) {
	WriteHertzText(h.c, code, msg)
}

func (h *hertzContext) SetStatusCode(code int) {
	h.c.SetStatusCode(code)
}

func (h *hertzContext) Flush() {
	_ = h.c.Flush()
}

func (h *hertzContext) Done() <-chan struct{} {
	if h != nil && h.ctx != nil {
		return h.ctx.Done()
	}
	return nil
}

func (h *hertzContext) ensureWriter() *sse.Writer {
	if h.writer == nil {
		h.writer = sse.NewWriter(h.c)
	}
	return h.writer
}

func (h *hertzContext) WriteSSEEvent(msg json.RawMessage) error {
	return writeHertzSSEEvent(h.ensureWriter(), msg)
}

func (h *hertzContext) WriteSSEKeepAlive() error {
	return h.ensureWriter().WriteKeepAlive()
}

func (h *hertzContext) CloseSSE() {
	if h.writer != nil {
		_ = h.writer.Close()
		h.writer = nil
	}
}

// WriteHertzText writes a plain text HTTP response on a Hertz request context.
func WriteHertzText(c *app.RequestContext, status int, body string) {
	c.SetStatusCode(status)
	c.SetBodyString(body)
}

// writeHertzSSEEvent writes a single SSE message event using a Hertz SSE writer.
func writeHertzSSEEvent(writer *sse.Writer, msg json.RawMessage) error {
	return writer.WriteEvent("", "message", msg)
}
