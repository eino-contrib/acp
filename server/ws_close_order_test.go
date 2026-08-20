package server

import (
	"context"
	"encoding/binary"
	"sync"
	"testing"
	"time"

	"github.com/eino-contrib/acp/internal/wsconn"
)

func TestServingWebSocketAdmissionAttemptsNormalCloseBeforeSocketClose(t *testing.T) {
	conn := &closeOrderWSConn{}
	admission := &WebSocketAdmission{
		conn: conn,
		wc: &wsConn{
			connCtx:    context.Background(),
			connCancel: func() {},
		},
	}
	admission.state.Store(admissionServing)

	admission.closeFromServer()

	events := conn.Events()
	if len(events) != 2 {
		t.Fatalf("close events = %v, want [close-control:1000 socket-close]", events)
	}
	if events[0] != "close-control:1000" || events[1] != "socket-close" {
		t.Fatalf("close events = %v, want [close-control:1000 socket-close]", events)
	}
}

type closeOrderWSConn struct {
	mu     sync.Mutex
	events []string
}

func (*closeOrderWSConn) ReadMessage() (int, []byte, error) { return 0, nil, nil }
func (*closeOrderWSConn) WriteMessage(int, []byte) error    { return nil }
func (c *closeOrderWSConn) WriteControl(messageType int, payload []byte, _ time.Time) error {
	if messageType == wsconn.CloseMessage && len(payload) >= 2 {
		c.record("close-control:" + closeCodeString(binary.BigEndian.Uint16(payload[:2])))
	}
	return nil
}
func (*closeOrderWSConn) SetReadLimit(int64)                {}
func (*closeOrderWSConn) SetReadDeadline(time.Time) error   { return nil }
func (*closeOrderWSConn) SetWriteDeadline(time.Time) error  { return nil }
func (*closeOrderWSConn) SetPingHandler(func(string) error) {}
func (c *closeOrderWSConn) Close() error {
	c.record("socket-close")
	return nil
}

func (c *closeOrderWSConn) record(event string) {
	c.mu.Lock()
	c.events = append(c.events, event)
	c.mu.Unlock()
}

func (c *closeOrderWSConn) Events() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.events...)
}

func closeCodeString(code uint16) string {
	if code == wsconn.CloseNormalClosure {
		return "1000"
	}
	return "unexpected"
}
