package httpserver

import (
	"context"
	"encoding/json"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/sse"
	acplog "github.com/eino-contrib/acp/internal/log"
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

// RequestBodyLimited reads at most maxBytes+1 bytes when Hertz exposes the
// request as a stream. Buffered requests are still copied so callers never
// retain Hertz-owned request memory. A non-positive limit means unlimited.
func (h *hertzContext) RequestBodyLimited(maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		return h.RequestBody()
	}
	if h.c.Request.IsBodyStream() {
		// Do not call Request.CloseBodyStream here. Hertz owns its inbound
		// bodyStream and, after the handler returns, ReleaseBodyStream drains
		// any unread bytes before the connection is reused. Clearing the stream
		// here after reading maxBytes+1 would prevent that drain and leave the
		// remaining chunk framing to be parsed as the next HTTP request.
		return readBodyLimited(h.c.Request.BodyStream(), maxBytes)
	}

	bodyBytes := h.c.Request.BodyBytes()
	if int64(len(bodyBytes)) > maxBytes {
		return nil, ErrRequestBodyTooLarge
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
	// Hertz's SSE Writer flushes each event/comment while holding its own
	// write mutex. Once the writer exists, a second raw RequestContext.Flush
	// is redundant and can race Writer.Close during session/close. Keep this
	// method active before SSE setup so response headers still flush promptly.
	if h.writer != nil {
		return
	}
	if err := h.c.Flush(); err != nil {
		acplog.CtxDebug(h.Context(), "flush hertz response: %v", err)
	}
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
		if err := h.writer.Close(); err != nil {
			acplog.CtxDebug(h.Context(), "close hertz sse writer: %v", err)
		}
	}
}

// WriteHertzText writes a plain text HTTP response on a Hertz request context.
func WriteHertzText(c *app.RequestContext, status int, body string) {
	c.Response.Header.SetContentType("text/plain; charset=utf-8")
	c.SetStatusCode(status)
	c.SetBodyString(body)
}

// writeHertzSSEEvent writes a single SSE message event using a Hertz SSE writer.
func writeHertzSSEEvent(writer *sse.Writer, msg json.RawMessage) error {
	return writer.WriteEvent("", "message", msg)
}
