package wsconn

import (
	"errors"
	"time"

	gorillawebsocket "github.com/gorilla/websocket"
)

type gorillaConn struct {
	conn *gorillawebsocket.Conn
}

// WrapGorilla adapts a Gorilla WebSocket connection to Conn. A nil input
// returns a nil Conn.
func WrapGorilla(conn *gorillawebsocket.Conn) Conn {
	if conn == nil {
		return nil
	}
	return &gorillaConn{conn: conn}
}

func (c *gorillaConn) ReadMessage() (int, []byte, error) {
	messageType, payload, err := c.conn.ReadMessage()
	return messageType, payload, normalizeGorillaError(err)
}

func (c *gorillaConn) WriteMessage(messageType int, payload []byte) error {
	return normalizeGorillaError(c.conn.WriteMessage(messageType, payload))
}

func (c *gorillaConn) WriteControl(messageType int, payload []byte, deadline time.Time) error {
	return normalizeGorillaError(c.conn.WriteControl(messageType, payload, deadline))
}

func (c *gorillaConn) SetReadLimit(limit int64) { c.conn.SetReadLimit(limit) }

func (c *gorillaConn) SetReadDeadline(deadline time.Time) error {
	return c.conn.SetReadDeadline(deadline)
}

func (c *gorillaConn) SetWriteDeadline(deadline time.Time) error {
	return c.conn.SetWriteDeadline(deadline)
}

func (c *gorillaConn) SetPingHandler(handler func(string) error) {
	c.conn.SetPingHandler(handler)
}

func (c *gorillaConn) Close() error { return normalizeGorillaError(c.conn.Close()) }

func normalizeGorillaError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, gorillawebsocket.ErrReadLimit) {
		return ErrReadLimit
	}
	var closeErr *gorillawebsocket.CloseError
	if errors.As(err, &closeErr) {
		return &CloseError{Code: closeErr.Code, Text: closeErr.Text}
	}
	return err
}

var _ Conn = (*gorillaConn)(nil)
