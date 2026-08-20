package wsupgrade

import (
	"errors"
	"net/http"
	"testing"
)

const validWebSocketKey = "dGhlIHNhbXBsZSBub25jZQ=="

func request(method string, headers map[string]string) Request {
	return Request{
		Method: method,
		Header: func(name string) string { return headers[http.CanonicalHeaderKey(name)] },
	}
}

func validRequest() Request {
	return request(http.MethodGet, map[string]string{
		"Connection":            "keep-alive, Upgrade",
		"Upgrade":               "WebSocket",
		"Sec-Websocket-Version": "13",
		"Sec-Websocket-Key":     validWebSocketKey,
	})
}

func TestIsAttempt(t *testing.T) {
	tests := []struct {
		name    string
		headers map[string]string
		want    bool
	}{
		{name: "complete", headers: map[string]string{"Connection": "keep-alive, Upgrade", "Upgrade": "websocket"}, want: true},
		{name: "connection signal only", headers: map[string]string{"Connection": "UPGRADE"}, want: true},
		{name: "upgrade signal only", headers: map[string]string{"Upgrade": "WebSocket"}, want: true},
		{name: "key signal only", headers: map[string]string{"Sec-Websocket-Key": validWebSocketKey}, want: true},
		{name: "version signal only", headers: map[string]string{"Sec-Websocket-Version": "13"}, want: true},
		{name: "substring is not token", headers: map[string]string{"Connection": "x-upgrade", "Upgrade": "websocket-v2"}},
		{name: "ordinary HTTP", headers: map[string]string{"Accept": "text/event-stream"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsAttempt(request(http.MethodGet, tt.headers)); got != tt.want {
				t.Fatalf("IsAttempt() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	if err := Validate(validRequest()); err != nil {
		t.Fatalf("Validate(valid request) error = %v", err)
	}

	tests := []struct {
		name string
		req  Request
		want error
	}{
		{name: "method", req: request(http.MethodPost, nil), want: ErrMethod},
		{name: "method is case sensitive", req: request("get", nil), want: ErrMethod},
		{name: "connection", req: request(http.MethodGet, map[string]string{"Upgrade": "websocket"}), want: ErrConnection},
		{name: "upgrade", req: request(http.MethodGet, map[string]string{"Connection": "upgrade"}), want: ErrUpgrade},
		{name: "version", req: request(http.MethodGet, map[string]string{"Connection": "upgrade", "Upgrade": "websocket", "Sec-Websocket-Version": "12"}), want: ErrVersion},
		{name: "missing key", req: request(http.MethodGet, map[string]string{"Connection": "upgrade", "Upgrade": "websocket", "Sec-Websocket-Version": "13"}), want: ErrKey},
		{name: "malformed key", req: request(http.MethodGet, map[string]string{"Connection": "upgrade", "Upgrade": "websocket", "Sec-Websocket-Version": "13", "Sec-Websocket-Key": "%%%"}), want: ErrKey},
		{name: "wrong decoded key length", req: request(http.MethodGet, map[string]string{"Connection": "upgrade", "Upgrade": "websocket", "Sec-Websocket-Version": "13", "Sec-Websocket-Key": "c2hvcnQ="}), want: ErrKey},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Validate(tt.req)
			if !errors.Is(err, tt.want) {
				t.Fatalf("Validate() error = %v, want errors.Is(_, %v)", err, tt.want)
			}
		})
	}
}

func TestValidateNilHeaderGetter(t *testing.T) {
	err := Validate(Request{Method: http.MethodGet})
	if !errors.Is(err, ErrConnection) {
		t.Fatalf("Validate(nil Header) error = %v, want ErrConnection", err)
	}
}

func TestValidateUsesAllRepeatedHeaderValues(t *testing.T) {
	req := Request{
		Method: http.MethodGet,
		Header: func(name string) string {
			switch http.CanonicalHeaderKey(name) {
			case "Sec-Websocket-Version":
				return "13"
			case "Sec-Websocket-Key":
				return validWebSocketKey
			default:
				return ""
			}
		},
		HeaderValues: func(name string) []string {
			switch http.CanonicalHeaderKey(name) {
			case "Connection":
				return []string{"keep-alive", "Upgrade"}
			case "Upgrade":
				return []string{"h2c", "websocket"}
			default:
				return nil
			}
		},
	}
	if err := Validate(req); err != nil {
		t.Fatalf("Validate(repeated headers): %v", err)
	}
}
