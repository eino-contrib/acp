package server

import (
	"context"
	"fmt"

	acpconn "github.com/eino-contrib/acp/conn"
	"github.com/eino-contrib/acp/internal/jsonrpc"
	acplog "github.com/eino-contrib/acp/internal/log"
	"github.com/eino-contrib/acp/internal/wsserver"
	"github.com/google/uuid"
	"github.com/hertz-contrib/websocket"
)

type wsConn struct {
	id         string
	connCtx    context.Context
	connCancel context.CancelFunc
	transport  *wsserver.Transport
	agentConn  *acpconn.AgentConnection
	server     *ACPServer
}

func (s *ACPServer) newWSConn(parent context.Context) (*wsConn, error) {
	if s.factory == nil {
		return nil, fmt.Errorf("agent factory is required")
	}

	parentCtx := connectionParentContext(parent, s.rootCtx)
	connCtx, connCancel := context.WithCancel(parentCtx)

	wsTransport := wsserver.New()

	agent := s.factory(connCtx)
	connID := uuid.NewString()
	opts := []jsonrpc.ConnectionOption{
		jsonrpc.WithMaxConsecutiveParseErrors(10),
		jsonrpc.WithConnectionLabel(connID),
	}
	if s.requestTimeout > 0 {
		opts = append(opts, jsonrpc.WithRequestTimeout(s.requestTimeout))
	}
	if s.notificationErrorHandler != nil {
		opts = append(opts, jsonrpc.WithNotificationErrorHandler(s.notificationErrorHandler))
	}
	agentConn := acpconn.NewAgentConnectionFromTransport(agent, wsTransport, opts...)
	if aware, ok := agent.(ConnectionAwareAgent); ok {
		aware.SetClientConnection(agentConn)
	}

	wc := &wsConn{
		id:         connID,
		connCtx:    connCtx,
		connCancel: connCancel,
		transport:  wsTransport,
		agentConn:  agentConn,
		server:     s,
	}

	// Start the read loop with the connection-level context (long-lived).
	// Start spawns the read loop and returns once the connection is ready.
	if err := agentConn.Start(connCtx); err != nil {
		wc.Close()
		return nil, fmt.Errorf("start agent connection: %w", err)
	}

	return wc, nil
}

func (wc *wsConn) Close() {
	wc.connCancel()
	if err := wc.agentConn.Close(); err != nil {
		acplog.CtxDebug(wc.connCtx, "close websocket connection %s: %v", wc.id, err)
	}
}

// Serve drives the WebSocket reader/writer using the connection-level
// context. The HTTP handler's request ctx is intentionally NOT used here: Hertz
// may cancel it once the upgrade handler returns, which would terminate an
// otherwise-healthy long-lived WS connection.
func (wc *wsConn) Serve(wsConn *websocket.Conn) {
	wc.transport.ServeConn(wc.connCtx, wsConn)
}
