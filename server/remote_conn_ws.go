package server

import (
	"context"
	"fmt"

	acpconn "github.com/eino-contrib/acp/conn"
	"github.com/eino-contrib/acp/internal/jsonrpc"
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
	wsTransport.Logger = s.logger

	agent := s.factory(connCtx)
	connID := uuid.NewString()
	opts := []jsonrpc.ConnectionOption{
		jsonrpc.WithMaxConsecutiveParseErrors(10),
		jsonrpc.WithLogger(s.logger),
		jsonrpc.WithConnectionLabel(connID),
	}
	if s.requestTimeout > 0 {
		opts = append(opts, jsonrpc.WithRequestTimeout(s.requestTimeout))
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
		wc.server.logger.CtxError(wc.connCtx, "close websocket connection %s: %v", wc.id, err)
	}
}

func (wc *wsConn) Serve(ctx context.Context, wsConn *websocket.Conn) {
	wc.transport.ServeConn(ctx, wsConn)
}
