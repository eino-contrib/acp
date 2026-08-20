package httpserver

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestValidatePostHeadersAcceptQualityAndPrecedence(t *testing.T) {
	tests := []struct {
		name       string
		accept     string
		wantStatus int
	}{
		{
			name:   "missing Accept preserves POST compatibility",
			accept: "",
		},
		{
			name:   "positive qvalues",
			accept: "application/json;q=0.25, text/event-stream;q=0.001",
		},
		{
			name:       "JSON q zero",
			accept:     "application/json;q=0, text/event-stream",
			wantStatus: http.StatusNotAcceptable,
		},
		{
			name:       "SSE q zero",
			accept:     "application/json, text/event-stream;q=0.000",
			wantStatus: http.StatusNotAcceptable,
		},
		{
			name:       "specific q zero overrides positive wildcard",
			accept:     "*/*;q=1, application/json;q=0, text/event-stream",
			wantStatus: http.StatusNotAcceptable,
		},
		{
			name:   "positive specifics override zero wildcard",
			accept: "*/*;q=0, application/json;q=1, text/event-stream;q=0.5",
		},
		{
			name:       "type wildcard q zero overrides positive full wildcard",
			accept:     "*/*;q=1, text/*;q=0, application/json",
			wantStatus: http.StatusNotAcceptable,
		},
		{
			name:       "malformed qvalue",
			accept:     "application/json;q=bogus, text/event-stream",
			wantStatus: http.StatusNotAcceptable,
		},
		{
			name:       "empty qvalue",
			accept:     "application/json, text/event-stream;q=\"\"",
			wantStatus: http.StatusNotAcceptable,
		},
		{
			name:       "out of range qvalue",
			accept:     "application/json, text/event-stream;q=1.001",
			wantStatus: http.StatusNotAcceptable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, gotStatus := ValidatePostHeaders("application/json", tt.accept)
			if gotStatus != tt.wantStatus {
				t.Fatalf("ValidatePostHeaders(_, %q) status = %d, want %d", tt.accept, gotStatus, tt.wantStatus)
			}
		})
	}
}

type getAcceptTrackingContext struct {
	*stubHandlerContext
	flushes        int
	keepAliveCalls int
	closeSSECalls  int
}

func (c *getAcceptTrackingContext) Flush() {
	c.flushes++
}

func (c *getAcceptTrackingContext) WriteSSEKeepAlive() error {
	c.keepAliveCalls++
	return nil
}

func (c *getAcceptTrackingContext) CloseSSE() {
	c.closeSSECalls++
}

func (c *getAcceptTrackingContext) WriteSSEEvent(msg json.RawMessage) error {
	return c.stubHandlerContext.WriteSSEEvent(msg)
}

func TestHandleProtocolGetRejectsUnacceptableAcceptBeforeLookupOrSSE(t *testing.T) {
	tests := []struct {
		name   string
		accept string
	}{
		{name: "missing"},
		{name: "wrong media type", accept: "application/json"},
		{name: "SSE q zero", accept: "*/*;q=1, text/event-stream;q=0"},
		{name: "malformed SSE qvalue", accept: "text/event-stream;q=nope"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lookupCalls := 0
			server := ProtocolServer{
				LookupConnection: func(string) (*ProtocolConnection, bool) {
					lookupCalls++
					return nil, false
				},
			}
			base := newStubHandlerContext()
			base.requestHeaders["Accept"] = tt.accept
			ctx := &getAcceptTrackingContext{stubHandlerContext: base}

			HandleProtocolGet(ctx, server)

			if ctx.statusCode != http.StatusNotAcceptable {
				t.Fatalf("status = %d, want %d", ctx.statusCode, http.StatusNotAcceptable)
			}
			if ctx.errMessage != "Accept must include text/event-stream" {
				t.Fatalf("error = %q, want Accept requirement", ctx.errMessage)
			}
			if lookupCalls != 0 {
				t.Fatalf("connection lookup calls = %d, want 0", lookupCalls)
			}
			if ctx.flushes != 0 || ctx.keepAliveCalls != 0 || ctx.closeSSECalls != 0 || len(ctx.sseEvents) != 0 {
				t.Fatalf("SSE started: flushes=%d keepalives=%d closes=%d events=%d",
					ctx.flushes, ctx.keepAliveCalls, ctx.closeSSECalls, len(ctx.sseEvents))
			}
			if got := ctx.response["Content-Type"]; got != "" {
				t.Fatalf("Content-Type = %q, want unset", got)
			}
		})
	}
}
