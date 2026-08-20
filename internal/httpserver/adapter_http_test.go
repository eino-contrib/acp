package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
)

type countingReader struct {
	r io.Reader
	n int
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	r.n += n
	return n, err
}

func TestHTTPHandlerContextRequestBodyLimited(t *testing.T) {
	t.Run("exact limit", func(t *testing.T) {
		reader := &countingReader{r: strings.NewReader("abcd")}
		req := httptest.NewRequest(http.MethodPost, "/", reader)
		ctx := NewHTTPHandlerContext(httptest.NewRecorder(), req)

		body, err := ctx.RequestBodyLimited(4)
		if err != nil {
			t.Fatalf("RequestBodyLimited() error = %v", err)
		}
		if got := string(body); got != "abcd" {
			t.Fatalf("body = %q, want %q", got, "abcd")
		}
	})

	t.Run("over limit reads only one extra byte", func(t *testing.T) {
		reader := &countingReader{r: strings.NewReader("abcdefgh")}
		req := httptest.NewRequest(http.MethodPost, "/", reader)
		ctx := NewHTTPHandlerContext(httptest.NewRecorder(), req)

		_, err := ctx.RequestBodyLimited(4)
		if !errors.Is(err, ErrRequestBodyTooLarge) {
			t.Fatalf("error = %v, want ErrRequestBodyTooLarge", err)
		}
		if reader.n != 5 {
			t.Fatalf("bytes read = %d, want max+1 = 5", reader.n)
		}
	})
}

func TestHertzHandlerContextRequestBodyLimited(t *testing.T) {
	t.Run("stream over limit reads only one extra byte", func(t *testing.T) {
		reader := &countingReader{r: strings.NewReader("abcdefgh")}
		requestContext := app.NewContext(0)
		requestContext.Request.SetBodyStream(reader, -1)
		ctx := NewHertzHandlerContext(context.Background(), requestContext)

		_, err := ctx.RequestBodyLimited(4)
		if !errors.Is(err, ErrRequestBodyTooLarge) {
			t.Fatalf("error = %v, want ErrRequestBodyTooLarge", err)
		}
		if reader.n != 5 {
			t.Fatalf("bytes read = %d, want max+1 = 5", reader.n)
		}
		if !requestContext.Request.IsBodyStream() {
			t.Fatal("RequestBodyLimited cleared Hertz-owned body stream before the host could drain it")
		}
	})

	t.Run("buffered body is copied", func(t *testing.T) {
		requestContext := app.NewContext(0)
		requestContext.Request.SetBodyString("abcd")
		ctx := NewHertzHandlerContext(context.Background(), requestContext)

		body, err := ctx.RequestBodyLimited(4)
		if err != nil {
			t.Fatalf("RequestBodyLimited() error = %v", err)
		}
		body[0] = 'z'
		if got := string(requestContext.Request.BodyBytes()); got != "abcd" {
			t.Fatalf("mutating returned body changed Hertz buffer to %q", got)
		}
	})
}

func TestHTTPHandlerContextSSEEncodingAndFlush(t *testing.T) {
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	ctx := NewHTTPHandlerContext(recorder, req)

	if err := ctx.WriteSSEEvent(json.RawMessage("first\r\nsecond\nthird")); err != nil {
		t.Fatalf("WriteSSEEvent() error = %v", err)
	}
	want := "event: message\ndata: first\ndata: second\ndata: third\n\n"
	if got := recorder.Body.String(); got != want {
		t.Fatalf("SSE body = %q, want %q", got, want)
	}
	if !recorder.Flushed {
		t.Fatal("WriteSSEEvent() did not flush the response")
	}
}

func TestHTTPHandlerContextSSEKeepAliveMatchesHertz(t *testing.T) {
	recorder := httptest.NewRecorder()
	ctx := NewHTTPHandlerContext(recorder, httptest.NewRequest(http.MethodGet, "/", nil))

	if err := ctx.WriteSSEKeepAlive(); err != nil {
		t.Fatalf("WriteSSEKeepAlive() error = %v", err)
	}
	if got, want := recorder.Body.String(), ":keep-alive\n"; got != want {
		t.Fatalf("keepalive body = %q, want %q", got, want)
	}
	if !recorder.Flushed {
		t.Fatal("WriteSSEKeepAlive() did not flush the response")
	}
}

type limitedStubHandlerContext struct {
	*stubHandlerContext
	maxBytes int64
	err      error
}

func (c *limitedStubHandlerContext) RequestBodyLimited(maxBytes int64) ([]byte, error) {
	c.maxBytes = maxBytes
	return nil, c.err
}

func TestHandleProtocolPostMapsLimitedBodyErrorTo413(t *testing.T) {
	base := newStubHandlerContext()
	base.requestHeaders["Content-Type"] = "application/json"
	base.requestHeaders["Accept"] = "application/json, text/event-stream"
	ctx := &limitedStubHandlerContext{
		stubHandlerContext: base,
		err:                ErrRequestBodyTooLarge,
	}

	HandleProtocolPost(ctx, ProtocolServer{MaxMessageSize: 4})

	if ctx.maxBytes != 4 {
		t.Fatalf("body limit = %d, want 4", ctx.maxBytes)
	}
	if ctx.statusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", ctx.statusCode, http.StatusRequestEntityTooLarge)
	}
	if ctx.errMessage != ErrRequestBodyTooLarge.Error() {
		t.Fatalf("message = %q, want %q", ctx.errMessage, ErrRequestBodyTooLarge.Error())
	}
}

func TestHTTPHandlerContextUsesRequestContext(t *testing.T) {
	type key struct{}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = req.WithContext(context.WithValue(req.Context(), key{}, "trace"))
	ctx := NewHTTPHandlerContext(httptest.NewRecorder(), req)
	if got := ctx.Context().Value(key{}); got != "trace" {
		t.Fatalf("context value = %#v, want trace", got)
	}
}

func TestTextErrorsUseSameContentTypeAcrossAdapters(t *testing.T) {
	recorder := httptest.NewRecorder()
	httpCtx := NewHTTPHandlerContext(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	httpCtx.WriteError(http.StatusBadRequest, "bad request")

	hertzCtx := app.NewContext(0)
	WriteHertzText(hertzCtx, http.StatusBadRequest, "bad request")

	const want = "text/plain; charset=utf-8"
	if got := recorder.Header().Get("Content-Type"); got != want {
		t.Fatalf("net/http Content-Type = %q, want %q", got, want)
	}
	if got := string(hertzCtx.Response.Header.ContentType()); got != want {
		t.Fatalf("Hertz Content-Type = %q, want %q", got, want)
	}
}
