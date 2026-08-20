package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// httpHandlerContext adapts a standard net/http request and response writer
// to HandlerContext. Gin uses this adapter through its underlying net/http
// request and response writer.
type httpHandlerContext struct {
	w http.ResponseWriter
	r *http.Request
}

// NewHTTPHandlerContext creates a HandlerContext backed by net/http.
func NewHTTPHandlerContext(w http.ResponseWriter, r *http.Request) *httpHandlerContext {
	return &httpHandlerContext{w: w, r: r}
}

func (h *httpHandlerContext) Context() context.Context {
	if h != nil && h.r != nil {
		return h.r.Context()
	}
	return context.Background()
}

func (h *httpHandlerContext) RequestHeader(key string) string {
	if h == nil || h.r == nil {
		return ""
	}
	if strings.EqualFold(key, "Content-Length") && h.r.ContentLength >= 0 {
		return strconv.FormatInt(h.r.ContentLength, 10)
	}
	return h.r.Header.Get(key)
}

func (h *httpHandlerContext) RequestBody() ([]byte, error) {
	if h == nil || h.r == nil || h.r.Body == nil {
		return nil, nil
	}
	return io.ReadAll(h.r.Body)
}

// RequestBodyLimited consumes at most maxBytes+1 bytes from the request. A
// non-positive maxBytes retains RequestBody's unlimited behavior.
func (h *httpHandlerContext) RequestBodyLimited(maxBytes int64) ([]byte, error) {
	if h == nil || h.r == nil || h.r.Body == nil {
		return nil, nil
	}
	return readBodyLimited(h.r.Body, maxBytes)
}

func (h *httpHandlerContext) SetResponseHeader(key, value string) {
	h.w.Header().Set(key, value)
}

func (h *httpHandlerContext) WriteError(code int, msg string) {
	h.w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	h.w.WriteHeader(code)
	_, _ = io.WriteString(h.w, msg)
}

func (h *httpHandlerContext) SetStatusCode(code int) {
	h.w.WriteHeader(code)
}

func (h *httpHandlerContext) Flush() {
	_ = http.NewResponseController(h.w).Flush()
}

func (h *httpHandlerContext) Done() <-chan struct{} {
	if h == nil || h.r == nil {
		return nil
	}
	return h.r.Context().Done()
}

func (h *httpHandlerContext) WriteSSEEvent(msg json.RawMessage) error {
	if err := writeHTTPMessageEvent(h.w, msg); err != nil {
		return err
	}
	return http.NewResponseController(h.w).Flush()
}

func (h *httpHandlerContext) WriteSSEKeepAlive() error {
	if _, err := io.WriteString(h.w, ":keep-alive\n"); err != nil {
		return err
	}
	return http.NewResponseController(h.w).Flush()
}

// CloseSSE is a no-op for net/http: the server owns the response stream and
// closes it when the handler returns.
func (h *httpHandlerContext) CloseSSE() {}

// writeHTTPMessageEvent mirrors Hertz's SSE encoding: CR, LF, and CRLF split
// data into separate fields, and an empty line terminates the message event.
func writeHTTPMessageEvent(w io.Writer, data []byte) error {
	if _, err := io.WriteString(w, "event: message\n"); err != nil {
		return err
	}
	for len(data) > 0 {
		i := bytes.IndexAny(data, "\r\n")
		line := data
		if i >= 0 {
			line = data[:i]
		}
		if _, err := io.WriteString(w, "data: "); err != nil {
			return err
		}
		if _, err := w.Write(line); err != nil {
			return err
		}
		if _, err := io.WriteString(w, "\n"); err != nil {
			return err
		}
		if i < 0 {
			data = nil
			continue
		}
		advance := i + 1
		if data[i] == '\r' && advance < len(data) && data[advance] == '\n' {
			advance++
		}
		data = data[advance:]
	}
	_, err := io.WriteString(w, "\n")
	return err
}
