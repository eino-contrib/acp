package main

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/cloudwego/hertz/pkg/protocol"
	"github.com/hertz-contrib/websocket"

	"github.com/eino-contrib/acp/internal/wsutil"
	"github.com/eino-contrib/acp/stream"
)

// wsStreamer is a minimal stream.Streamer implementation backed by a single
// WebSocket connection. It is shared by both the Proxy-side dialer (see
// factory.go) and the AgentServer-side handler (see agent_server.go).
//
// In production, users plug in their own RPC-specific Streamer — this type
// exists only so the example can run end-to-end without extra dependencies.
type wsStreamer struct {
	conn *websocket.Conn

	// hReq/hResp are the Hertz request/response objects retained by the
	// websocket connection's underlying reader. They are released back to the
	// Hertz pool in Close, AFTER the connection is torn down, so that the
	// reader cannot still be touching their buffers. Only set on the client
	// side of an Upgrade; the server side leaves them nil.
	hReq  *protocol.Request
	hResp *protocol.Response

	writeMu      sync.Mutex
	writeTimeout time.Duration

	closeOnce sync.Once
	closeErr  error
	closed    chan struct{}
}

var _ stream.Streamer = (*wsStreamer)(nil)

func newWSStreamer(conn *websocket.Conn, writeTimeout time.Duration) *wsStreamer {
	return &wsStreamer{
		conn:         conn,
		writeTimeout: writeTimeout,
		closed:       make(chan struct{}),
	}
}

// newClientWSStreamer wraps a just-upgraded client-side websocket conn and
// takes ownership of the Hertz request/response objects, which must stay
// alive for as long as the connection is reading.
func newClientWSStreamer(conn *websocket.Conn, req *protocol.Request, resp *protocol.Response, writeTimeout time.Duration) *wsStreamer {
	s := newWSStreamer(conn, writeTimeout)
	s.hReq = req
	s.hResp = resp
	return s
}

func (s *wsStreamer) WritePayload(ctx context.Context, payload []byte) error {
	select {
	case <-s.closed:
		return io.ErrClosedPipe
	default:
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	deadline := time.Time{}
	if d, ok := ctx.Deadline(); ok {
		deadline = d
	} else if s.writeTimeout > 0 {
		deadline = time.Now().Add(s.writeTimeout)
	}
	if !deadline.IsZero() {
		if err := s.conn.SetWriteDeadline(deadline); err != nil {
			return err
		}
	}
	return s.conn.WriteMessage(websocket.TextMessage, payload)
}

func (s *wsStreamer) ReadPayload(ctx context.Context) ([]byte, error) {
	// The websocket library does not accept a ctx on ReadMessage. Callers
	// that need prompt ctx cancellation should arrange to call Close on the
	// streamer from the cancellation path. The proxy's down-pump only
	// cancels via Close, so this is sufficient for the example.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for {
		msgType, data, err := s.conn.ReadMessage()
		if err != nil {
			return nil, normalizeCloseErr(err)
		}
		switch msgType {
		case websocket.TextMessage, websocket.BinaryMessage:
			return data, nil
		default:
			// Control frames are handled inside the websocket library; skip.
			continue
		}
	}
}

func (s *wsStreamer) Close(reason string) error {
	s.closeOnce.Do(func() {
		// Best-effort close frame so the peer can log the reason.
		s.writeMu.Lock()
		_ = s.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
		_ = s.conn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, wsutil.SafeCloseReason(reason)))
		s.writeMu.Unlock()
		s.closeErr = s.conn.Close()
		close(s.closed)

		// Release pooled Hertz objects only after the connection is closed
		// so the reader can no longer touch their buffers.
		if s.hReq != nil {
			protocol.ReleaseRequest(s.hReq)
			s.hReq = nil
		}
		if s.hResp != nil {
			protocol.ReleaseResponse(s.hResp)
			s.hResp = nil
		}
	})
	return s.closeErr
}

// normalizeCloseErr converts expected EOF / close-frame events into io.EOF so
// the NewPipe adapter can signal a graceful shutdown to the stdio transport.
func normalizeCloseErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, io.EOF) {
		return io.EOF
	}
	if _, ok := err.(*websocket.CloseError); ok {
		return io.EOF
	}
	return err
}
