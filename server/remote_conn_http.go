package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	acpconn "github.com/eino-contrib/acp/conn"
	"github.com/eino-contrib/acp/internal/connspi"
	acphttpserver "github.com/eino-contrib/acp/internal/httpserver"
	"github.com/eino-contrib/acp/internal/jsonrpc"
	acplog "github.com/eino-contrib/acp/internal/log"
)

type httpRemoteConnection struct {
	id         string
	logger     acplog.Logger
	conn       *acpconn.AgentConnection
	connCancel context.CancelFunc
	done       chan struct{}
	once       sync.Once
	closeErr   error // cached error from the first Close call

	httpConn     *acphttpserver.Connection
	pendingReqs  *acphttpserver.PendingRequests
	sender       *httpAgentSender
	mergedDone   <-chan struct{}
	protocolConn *acphttpserver.ProtocolConnection // cached, created once
}

func (c *httpRemoteConnection) IsIdle(timeout time.Duration) bool {
	if c.httpConn == nil {
		return false
	}
	return c.httpConn.IsIdle(timeout)
}

func (c *httpRemoteConnection) Logger() acplog.Logger {
	if c.logger != nil {
		return c.logger
	}
	return acplog.Default()
}

func (c *httpRemoteConnection) ProtocolConnection() *acphttpserver.ProtocolConnection {
	return c.protocolConn
}

func (c *httpRemoteConnection) Close() error {
	c.once.Do(func() {
		if c.connCancel != nil {
			c.connCancel()
		}
		close(c.done)

		if c.pendingReqs != nil {
			c.pendingReqs.Close()
		}

		var errs []error
		if c.sender != nil {
			if err := c.sender.Close(); err != nil {
				errs = append(errs, fmt.Errorf("close HTTP sender %s: %w", c.id, err))
			}
		}

		if err := c.conn.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close agent connection %s: %w", c.id, err))
		}

		if c.httpConn != nil {
			acphttpserver.CloseConnection(c.httpConn)
		}

		c.closeErr = errors.Join(errs...)
	})
	return c.closeErr
}

// httpAgentSender implements connspi.Sender for the HTTP direct-dispatch path.
//
// Notifications are always sent via GET SSE session listeners.
// Requests (agent reverse calls) are sent via GET SSE and awaited via PendingRequests.
type httpAgentSender struct {
	httpConn    *acphttpserver.Connection
	pendingReqs *acphttpserver.PendingRequests
	logger      acplog.Logger
	nextID      atomic.Int64
	done        chan struct{}
	closed      atomic.Bool
}

// SendNotification sends a JSON-RPC notification to the client via GET SSE.
func (s *httpAgentSender) SendNotification(ctx context.Context, method string, params any) error {
	msg, err := jsonrpc.NewNotification(method, params)
	if err != nil {
		return fmt.Errorf("build notification: %w", err)
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal notification: %w", err)
	}
	sessionID, err := resolveSessionID(ctx, params, "notification", method)
	if err != nil {
		return err
	}
	return s.writeToGETSSE(sessionID, data)
}

// SendRequest sends a JSON-RPC request to the client (agent reverse call) and
// blocks until the client responds via POST.
func (s *httpAgentSender) SendRequest(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := jsonrpc.NewIntID(s.nextID.Add(1))
	idKey := id.String()

	msg, err := jsonrpc.NewRequest(id, method, params)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	ch := s.pendingReqs.Register(idKey)
	if ch == nil {
		return nil, fmt.Errorf("send reverse request %s: connection closed", method)
	}
	defer s.pendingReqs.Cancel(idKey)

	sessionID, err := resolveSessionID(ctx, params, "reverse request", method)
	if err != nil {
		return nil, err
	}
	if err := s.writeToGETSSE(sessionID, data); err != nil {
		return nil, fmt.Errorf("send reverse request via GET SSE: %w", err)
	}

	select {
	case resp, ok := <-ch:
		if !ok {
			return nil, fmt.Errorf("pending request cancelled")
		}
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.done:
		return nil, fmt.Errorf("HTTP sender closed")
	}
}

// resolveSessionID pulls the session ID from params first, then falls back to
// the session-scoped context installed by the HTTP dispatcher. Returns an
// error describing the routing requirement when neither source has it.
func resolveSessionID(ctx context.Context, params any, kind, method string) (string, error) {
	sessionID := connspi.ExtractSessionIDFromAny(params)
	if sessionID == "" {
		sessionID = connspi.SessionIDFromContext(ctx)
	}
	if sessionID == "" {
		return "", fmt.Errorf("send %s %s: no session ID in params or context (Streamable HTTP requires sessionId for routing; include it in params or call from a session-scoped handler)", kind, method)
	}
	return sessionID, nil
}

// Done returns a channel that is closed when the sender is no longer usable.
func (s *httpAgentSender) Done() <-chan struct{} {
	return s.done
}

// Close shuts down the sender.
func (s *httpAgentSender) Close() error {
	if s.closed.CompareAndSwap(false, true) {
		close(s.done)
	}
	return nil
}

func (s *httpAgentSender) writeToGETSSE(sessionID string, data json.RawMessage) error {
	sess, ok := s.httpConn.LookupSession(sessionID)
	if !ok {
		return fmt.Errorf("unknown session %s", sessionID)
	}
	return sess.Send(data)
}
