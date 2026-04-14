package server

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	acpconn "github.com/eino-contrib/acp/conn"
	acphttpserver "github.com/eino-contrib/acp/internal/httpserver"
)

func (s *ACPServer) newHTTPConnection(parent context.Context) (*httpRemoteConnection, error) {
	s.conns.startReaper(s.rootCtx)
	if s.factory == nil {
		return nil, fmt.Errorf("agent factory is required")
	}

	parentCtx := connectionParentContext(parent, s.rootCtx)
	connCtx, connCancel := context.WithCancel(parentCtx)

	httpConn := acphttpserver.NewConnection()
	httpConn.Logger = s.logger
	httpConn.PendingQueueSize = s.pendingQueueSize

	pendingReqs := acphttpserver.NewPendingRequests()

	connID := uuid.NewString()
	httpConn.ConnectionID = connID

	c := &httpRemoteConnection{
		id:          connID,
		logger:      s.logger,
		connCancel:  connCancel,
		done:        make(chan struct{}),
		httpConn:    httpConn,
		pendingReqs: pendingReqs,
	}
	c.mergedDone = acphttpserver.MergeDone(c.done, s.done)

	agent := s.factory(connCtx)

	// Create httpAgentSender — implements connspi.Sender directly.
	sender := &httpAgentSender{
		httpConn:    httpConn,
		pendingReqs: pendingReqs,
		logger:      s.logger,
		done:        make(chan struct{}),
	}
	c.sender = sender

	// Create AgentConnection with the sender (no jsonrpc.Connection).
	c.conn = acpconn.NewAgentConnection(agent, sender)
	if aware, ok := agent.(ConnectionAwareAgent); ok {
		aware.SetClientConnection(c.conn)
	}

	// Cache the ProtocolConnection so we don't recreate it per request.
	c.protocolConn = acphttpserver.NewProtocolConnection(acphttpserver.ProtocolConnectionConfig{
		ConnectionID: c.id,
		HTTPConn:     c.httpConn,
		Dispatcher:   c.conn.InboundDispatcher(),
		PendingReqs:  c.pendingReqs,
		Done:         c.mergedDone,
		Close:        c.Close,
	})

	s.conns.add(c)
	return c, nil
}

func connectionParentContext(parent, fallback context.Context) context.Context {
	switch {
	case parent != nil:
		return context.WithoutCancel(parent)
	case fallback != nil:
		return fallback
	default:
		return context.Background()
	}
}
