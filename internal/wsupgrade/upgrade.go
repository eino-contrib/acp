// Package wsupgrade contains framework-neutral WebSocket handshake checks.
package wsupgrade

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

var (
	ErrMethod     = errors.New("websocket upgrade requires GET")
	ErrConnection = errors.New("websocket upgrade requires Connection: upgrade")
	ErrUpgrade    = errors.New("websocket upgrade requires Upgrade: websocket")
	ErrVersion    = errors.New("websocket upgrade requires Sec-WebSocket-Version: 13")
	ErrKey        = errors.New("websocket upgrade requires a valid 16-byte Sec-WebSocket-Key")
)

// Request is the handshake input shared by Hertz and net/http adapters.
// Header must return the value for a case-insensitive HTTP header name.
type Request struct {
	Method string
	Header func(name string) string
	// HeaderValues optionally returns every field-line value for name. When
	// set, token validation considers all values so repeated HTTP headers keep
	// their normal comma-list semantics.
	HeaderValues func(name string) []string
}

// IsAttempt reports whether the request carries any WebSocket opening-
// handshake signal. It intentionally accepts incomplete attempts so malformed
// handshakes are rejected by Validate instead of being routed as ordinary
// HTTP/SSE requests.
func IsAttempt(req Request) bool {
	return req.headerHasToken("Connection", "upgrade") ||
		req.headerHasToken("Upgrade", "websocket") ||
		strings.TrimSpace(req.header("Sec-WebSocket-Key")) != "" ||
		strings.TrimSpace(req.header("Sec-WebSocket-Version")) != ""
}

// Validate validates the RFC 6455 HTTP/1.1 opening handshake fields shared by
// all supported server frameworks.
func Validate(req Request) error {
	if req.Method != http.MethodGet {
		return fmt.Errorf("%w: got %q", ErrMethod, req.Method)
	}
	if !req.headerHasToken("Connection", "upgrade") {
		return ErrConnection
	}
	if !req.headerHasToken("Upgrade", "websocket") {
		return ErrUpgrade
	}
	if req.header("Sec-WebSocket-Version") != "13" {
		return ErrVersion
	}
	key := strings.TrimSpace(req.header("Sec-WebSocket-Key"))
	decoded, err := base64.StdEncoding.Strict().DecodeString(key)
	if err != nil || len(decoded) != 16 {
		return ErrKey
	}
	return nil
}

func (req Request) header(name string) string {
	if req.Header == nil {
		return ""
	}
	return req.Header(name)
}

func (req Request) headerHasToken(name, token string) bool {
	if req.HeaderValues != nil {
		if values := req.HeaderValues(name); len(values) > 0 {
			for _, value := range values {
				if headerHasToken(value, token) {
					return true
				}
			}
			return false
		}
	}
	return headerHasToken(req.header(name), token)
}

func headerHasToken(value, token string) bool {
	for _, part := range strings.Split(value, ",") {
		if strings.EqualFold(strings.TrimSpace(part), token) {
			return true
		}
	}
	return false
}
