package wsconn

import (
	"encoding/binary"
	"errors"
	"strconv"

	gorillawebsocket "github.com/gorilla/websocket"
	hertzwebsocket "github.com/hertz-contrib/websocket"
)

// Close codes defined by RFC 6455 section 11.7.
const (
	CloseNormalClosure           = 1000
	CloseGoingAway               = 1001
	CloseProtocolError           = 1002
	CloseUnsupportedData         = 1003
	CloseNoStatusReceived        = 1005
	CloseAbnormalClosure         = 1006
	CloseInvalidFramePayloadData = 1007
	ClosePolicyViolation         = 1008
	CloseMessageTooBig           = 1009
	CloseMandatoryExtension      = 1010
	CloseInternalServerErr       = 1011
	CloseServiceRestart          = 1012
	CloseTryAgainLater           = 1013
	CloseTLSHandshake            = 1015
)

// Message types defined by RFC 6455 section 11.8.
const (
	TextMessage   = 1
	BinaryMessage = 2
	CloseMessage  = 8
	PingMessage   = 9
	PongMessage   = 10
)

// ErrReadLimit is returned when an inbound message exceeds the connection's
// configured read limit. Both Hertz and Gorilla adapters normalize their
// library-specific sentinel to this value.
var ErrReadLimit = errors.New("websocket: read limit exceeded")

// CloseError represents a peer WebSocket close frame.
type CloseError struct {
	Code int
	Text string
}

// Error implements error.
func (e *CloseError) Error() string {
	if e == nil {
		return "websocket: close"
	}
	s := "websocket: close " + strconv.Itoa(e.Code)
	switch e.Code {
	case CloseNormalClosure:
		s += " (normal)"
	case CloseGoingAway:
		s += " (going away)"
	case CloseProtocolError:
		s += " (protocol error)"
	case CloseUnsupportedData:
		s += " (unsupported data)"
	case CloseNoStatusReceived:
		s += " (no status)"
	case CloseAbnormalClosure:
		s += " (abnormal closure)"
	case CloseInvalidFramePayloadData:
		s += " (invalid payload data)"
	case ClosePolicyViolation:
		s += " (policy violation)"
	case CloseMessageTooBig:
		s += " (message too big)"
	case CloseMandatoryExtension:
		s += " (mandatory extension missing)"
	case CloseInternalServerErr:
		s += " (internal server error)"
	case CloseTLSHandshake:
		s += " (TLS handshake error)"
	}
	if e.Text != "" {
		s += ": " + e.Text
	}
	return s
}

// AsCloseError extracts a normalized close error. It also accepts errors from
// the underlying Hertz and Gorilla libraries so callers remain robust at
// adapter boundaries and in tests.
func AsCloseError(err error) (*CloseError, bool) {
	if err == nil {
		return nil, false
	}
	var normalized *CloseError
	if errors.As(err, &normalized) {
		return normalized, true
	}
	var hertzErr *hertzwebsocket.CloseError
	if errors.As(err, &hertzErr) {
		return &CloseError{Code: hertzErr.Code, Text: hertzErr.Text}, true
	}
	var gorillaErr *gorillawebsocket.CloseError
	if errors.As(err, &gorillaErr) {
		return &CloseError{Code: gorillaErr.Code, Text: gorillaErr.Text}, true
	}
	return nil, false
}

// IsCloseError reports whether err is a close error with one of codes.
func IsCloseError(err error, codes ...int) bool {
	closeErr, ok := AsCloseError(err)
	if !ok {
		return false
	}
	for _, code := range codes {
		if closeErr.Code == code {
			return true
		}
	}
	return false
}

// IsUnexpectedCloseError reports whether err is a close error whose code is
// not included in expectedCodes.
func IsUnexpectedCloseError(err error, expectedCodes ...int) bool {
	closeErr, ok := AsCloseError(err)
	if !ok {
		return false
	}
	for _, code := range expectedCodes {
		if closeErr.Code == code {
			return false
		}
	}
	return true
}

// FormatCloseMessage formats an RFC 6455 close control-frame payload. The
// reserved CloseNoStatusReceived code is represented by an empty payload.
func FormatCloseMessage(code int, text string) []byte {
	if code == CloseNoStatusReceived {
		return []byte{}
	}
	payload := make([]byte, 2+len(text))
	binary.BigEndian.PutUint16(payload, uint16(code))
	copy(payload[2:], text)
	return payload
}
