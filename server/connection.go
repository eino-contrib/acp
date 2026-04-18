package server

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	acpconn "github.com/eino-contrib/acp/conn"
	"github.com/eino-contrib/acp/internal/connspi"
	acphttpserver "github.com/eino-contrib/acp/internal/httpserver"
	"github.com/eino-contrib/acp/internal/safe"
)

func (s *ACPServer) newHTTPConnection(parent context.Context) (*httpRemoteConnection, error) {
	s.conns.startReaper(s.rootCtx)
	if s.factory == nil {
		return nil, fmt.Errorf("agent factory is required")
	}

	parentCtx := connectionParentContext(parent, s.rootCtx)
	connCtx, connCancel := context.WithCancel(parentCtx)

	httpConn := acphttpserver.NewConnection()
	httpConn.PendingQueueSize = s.pendingQueueSize

	pendingReqs := acphttpserver.NewPendingRequests()

	connID := uuid.NewString()
	httpConn.ConnectionID = connID

	c := &httpRemoteConnection{
		id:          connID,
		connCancel:  connCancel,
		done:        make(chan struct{}),
		httpConn:    httpConn,
		pendingReqs: pendingReqs,
	}
	c.mergedDone = safe.MergeDone(c.done, s.done)

	agent := s.factory(connCtx)

	// Create httpAgentSender — implements connspi.Sender directly.
	sender := &httpAgentSender{
		httpConn:    httpConn,
		pendingReqs: pendingReqs,
		done:        make(chan struct{}),
	}
	c.sender = sender

	spiKey := connspi.NewAgentSPIKey()

	// Create AgentConnection with the sender (no jsonrpc.Connection).
	c.conn = acpconn.NewAgentConnectionSPI(spiKey, agent, sender)
	if aware, ok := agent.(ConnectionAwareAgent); ok {
		aware.SetClientConnection(c.conn)
	}

	// Cache the ProtocolConnection so we don't recreate it per request.
	c.protocolConn = acphttpserver.NewProtocolConnection(acphttpserver.ProtocolConnectionConfig{
		ConnectionID:        c.id,
		HTTPConn:            c.httpConn,
		Dispatcher:          c.conn.InboundDispatcherSPI(spiKey),
		PendingReqs:         c.pendingReqs,
		Done:                c.mergedDone,
		Close:               c.Close,
		MaxInflightDispatch: s.maxInflightDispatch,
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
