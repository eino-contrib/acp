package server

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	acpconn "github.com/eino-contrib/acp/conn"
	"github.com/eino-contrib/acp/internal/connspi"
	"github.com/eino-contrib/acp/internal/jsonrpc"
	acplog "github.com/eino-contrib/acp/internal/log"
	"github.com/eino-contrib/acp/internal/wsconn"
	"github.com/eino-contrib/acp/internal/wsserver"
	"github.com/eino-contrib/acp/internal/wsutil"
	"github.com/google/uuid"
)

type wsConn struct {
	id         string
	connCtx    context.Context
	connCancel context.CancelFunc
	transport  *wsserver.Transport
	agentConn  *acpconn.AgentConnection
	server     *ACPServer
	closeOnce  sync.Once
}

func (s *ACPServer) newWSConn(connCtx context.Context, connCancel context.CancelFunc, connID string) (_ *wsConn, err error) {
	if s.factory == nil {
		return nil, fmt.Errorf("agent factory is required")
	}

	if connID == "" {
		connID = uuid.NewString()
	}
	cleanup := true
	defer func() {
		if cleanup {
			connCancel()
		}
	}()

	wsTransport := wsserver.New(
		wsserver.WithReadTimeout(s.wsReadTimeout),
		wsserver.WithInitializeTimeout(s.wsInitializeTimeout),
	)

	agent, err := s.createAgent(connCtx)
	if err != nil {
		return nil, err
	}
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
	if err := setClientConnection(agent, agentConn); err != nil {
		_ = agentConn.Close()
		return nil, err
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

	cleanup = false
	return wc, nil
}

func (wc *wsConn) Close() {
	wc.closeOnce.Do(func() {
		wc.connCancel()
		if wc.agentConn != nil {
			if err := wc.agentConn.Close(); err != nil {
				acplog.CtxDebug(wc.connCtx, "close websocket connection %s: %v", wc.id, err)
			}
		}
	})
}

// Serve drives the WebSocket reader/writer using the connection-level
// context. The HTTP handler's request ctx is intentionally NOT used here: Hertz
// may cancel it once the upgrade handler returns, which would terminate an
// otherwise-healthy long-lived WS connection.
func (wc *wsConn) Serve(wsConn WebSocketConn) {
	wc.transport.ServeConn(wc.connCtx, wsConn)
}

const (
	admissionPending uint32 = iota
	admissionServing
	admissionClosing
	admissionAborted
)

// WebSocketAdmission reserves one server lifecycle slot while a framework
// adapter performs its WebSocket upgrade. A successful adapter calls Serve;
// a failed upgrade calls Abort. Both operations are safe to race and consume
// the admission at most once.
type WebSocketAdmission struct {
	server *ACPServer
	id     string
	parent context.Context
	state  atomic.Uint32
	mu     sync.Mutex
	cancel context.CancelFunc
	conn   WebSocketConn
	wc     *wsConn
	finish sync.Once
}

// AdmitWebSocket reserves a WebSocket connection before the framework begins
// its upgrade. AgentFactory is deliberately not called until Serve, after the
// upgrade has succeeded.
func (s *ACPServer) AdmitWebSocket(parent context.Context) (*WebSocketAdmission, error) {
	if s == nil {
		return nil, ErrServerClosed
	}
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.closing {
		return nil, ErrServerClosed
	}
	admission := &WebSocketAdmission{
		server: s,
		id:     uuid.NewString(),
		parent: parent,
	}
	s.active.Add(1)
	s.wsAdmissions[admission] = struct{}{}
	return admission, nil
}

// ConnectionID is the value an adapter places in the successful upgrade
// response.
func (a *WebSocketAdmission) ConnectionID() string {
	if a == nil {
		return ""
	}
	return a.id
}

// Abort releases an admission whose framework upgrade failed.
func (a *WebSocketAdmission) Abort() {
	if a == nil {
		return
	}
	for {
		switch state := a.state.Load(); state {
		case admissionPending, admissionClosing:
			if a.state.CompareAndSwap(state, admissionAborted) {
				a.complete()
				return
			}
		default:
			return
		}
	}
}

// Serve creates the connection-specific Agent only after a successful
// framework upgrade, registers it with the server lifecycle, and blocks until
// the WebSocket transport exits.
func (a *WebSocketAdmission) Serve(conn WebSocketConn) error {
	if a == nil || a.server == nil {
		if conn != nil {
			_ = conn.Close()
		}
		return ErrServerClosed
	}
	if conn == nil {
		a.Abort()
		return fmt.Errorf("websocket connection is nil")
	}

	// Transition while holding the server admission boundary. Close cannot
	// pass its closing fence until the upgraded socket and cancellation hook
	// are visible on the admission.
	a.server.lifecycleMu.Lock()
	if a.server.closing || !a.state.CompareAndSwap(admissionPending, admissionServing) {
		a.server.lifecycleMu.Unlock()
		_ = conn.Close()
		if a.state.Load() == admissionClosing {
			a.complete()
		}
		return ErrServerClosed
	}
	parentCtx := connectionParentContext(a.parent, a.server.rootCtx)
	parentCtx = connspi.WithConnectionID(parentCtx, a.id)
	connCtx, cancelConn := context.WithCancel(parentCtx)
	stopRootCancel := context.AfterFunc(a.server.rootCtx, cancelConn)
	connCancel := func() {
		stopRootCancel()
		cancelConn()
	}
	a.mu.Lock()
	a.conn = conn
	a.cancel = connCancel
	a.mu.Unlock()
	a.server.lifecycleMu.Unlock()
	defer a.complete()

	wc, err := a.server.newWSConn(connCtx, connCancel, a.id)
	if err != nil {
		acplog.CtxError(connCtx, "create websocket connection %s failed: %v", a.id, err)
		closeWebSocketAfterSetupFailure(conn)
		return err
	}
	a.mu.Lock()
	if a.state.Load() == admissionClosing {
		a.mu.Unlock()
		wc.Close()
		_ = conn.Close()
		return ErrServerClosed
	}
	a.wc = wc
	a.mu.Unlock()
	defer wc.Close()
	wc.Serve(conn)
	return nil
}

func closeWebSocketAfterSetupFailure(conn WebSocketConn) {
	if conn == nil {
		return
	}
	_ = conn.WriteControl(
		wsconn.CloseMessage,
		wsconn.FormatCloseMessage(wsconn.CloseInternalServerErr, "failed to create connection"),
		time.Now().Add(wsutil.ControlWriteDeadline),
	)
	_ = conn.Close()
}

func (a *WebSocketAdmission) complete() {
	if a == nil || a.server == nil {
		return
	}
	a.finish.Do(func() { a.server.releaseWSAdmission(a) })
}

// closeFromServer terminates both pending upgrades and upgraded connections.
// A serving admission is released by Serve only after its handler actually
// returns, so Shutdown never reports success while a WS handler is alive.
func (a *WebSocketAdmission) closeFromServer() {
	if a == nil {
		return
	}
	for {
		switch state := a.state.Load(); state {
		case admissionPending:
			if a.state.CompareAndSwap(admissionPending, admissionClosing) {
				return
			}
		case admissionServing:
			if !a.state.CompareAndSwap(admissionServing, admissionClosing) {
				continue
			}
			a.mu.Lock()
			cancel, wc, conn := a.cancel, a.wc, a.conn
			a.mu.Unlock()
			if conn != nil {
				_ = conn.WriteControl(
					wsconn.CloseMessage,
					wsconn.FormatCloseMessage(wsconn.CloseNormalClosure, ""),
					time.Now().Add(wsutil.ControlWriteDeadline),
				)
			}
			if cancel != nil {
				cancel()
			}
			if wc != nil {
				wc.Close()
			}
			if conn != nil {
				_ = conn.Close()
			}
			return
		default:
			return
		}
	}
}
