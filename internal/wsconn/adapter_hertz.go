package wsconn

import (
	"errors"
	"time"

	hertzwebsocket "github.com/hertz-contrib/websocket"
)

type hertzConn struct {
	conn *hertzwebsocket.Conn
}

// WrapHertz adapts a Hertz WebSocket connection to Conn. A nil input returns
// a nil Conn.
func WrapHertz(conn *hertzwebsocket.Conn) Conn {
	if conn == nil {
		return nil
	}
	return &hertzConn{conn: conn}
}

func (c *hertzConn) ReadMessage() (int, []byte, error) {
	messageType, payload, err := c.conn.ReadMessage()
	return messageType, payload, normalizeHertzError(err)
}

func (c *hertzConn) WriteMessage(messageType int, payload []byte) error {
	return normalizeHertzError(c.conn.WriteMessage(messageType, payload))
}

func (c *hertzConn) WriteControl(messageType int, payload []byte, deadline time.Time) error {
	return normalizeHertzError(c.conn.WriteControl(messageType, payload, deadline))
}

func (c *hertzConn) SetReadLimit(limit int64) { c.conn.SetReadLimit(limit) }

func (c *hertzConn) SetReadDeadline(deadline time.Time) error {
	return c.conn.SetReadDeadline(deadline)
}

func (c *hertzConn) SetWriteDeadline(deadline time.Time) error {
	return c.conn.SetWriteDeadline(deadline)
}

func (c *hertzConn) SetPingHandler(handler func(string) error) {
	c.conn.SetPingHandler(handler)
}

func (c *hertzConn) Close() error { return normalizeHertzError(c.conn.Close()) }

func normalizeHertzError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, hertzwebsocket.ErrReadLimit) {
		return ErrReadLimit
	}
	var closeErr *hertzwebsocket.CloseError
	if errors.As(err, &closeErr) {
		return &CloseError{Code: closeErr.Code, Text: closeErr.Text}
	}
	return err
}

var _ Conn = (*hertzConn)(nil)
