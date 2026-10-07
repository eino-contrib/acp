package conn

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	acp "github.com/eino-contrib/acp"
	acplog "github.com/eino-contrib/acp/internal/log"
)

type channelTransport struct {
	inbox  chan json.RawMessage
	outbox chan json.RawMessage
	closed chan struct{}
	once   sync.Once
}

func newChannelTransport() *channelTransport {
	return &channelTransport{
		inbox:  make(chan json.RawMessage, 16),
		outbox: make(chan json.RawMessage, 16),
		closed: make(chan struct{}),
	}
}

func (t *channelTransport) ReadMessage(context.Context) (json.RawMessage, error) {
	select {
	case msg := <-t.inbox:
		return msg, nil
	case <-t.closed:
		return nil, io.EOF
	}
}

func (t *channelTransport) WriteMessage(_ context.Context, data json.RawMessage) error {
	select {
	case t.outbox <- append(json.RawMessage(nil), data...):
		return nil
	case <-t.closed:
		return io.EOF
	}
}

func (t *channelTransport) Close() error {
	t.once.Do(func() {
		close(t.closed)
	})
	return nil
}

type blockingPromptAgent struct {
	acp.BaseAgent

	started       chan struct{}
	sessionCancel chan struct{}
}

func (a *blockingPromptAgent) Prompt(ctx context.Context, _ acp.PromptRequest) (acp.PromptResponse, error) {
	a.started <- struct{}{}
	<-ctx.Done()
	return acp.PromptResponse{}, ctx.Err()
}

func (a *blockingPromptAgent) SessionCancel(context.Context, acp.CancelNotification) error {
	select {
	case a.sessionCancel <- struct{}{}:
	default:
	}
	return nil
}

func TestAgentSideConnectionReportsUnknownNotification(t *testing.T) {
	transport := newChannelTransport()
	agent := &blockingPromptAgent{
		started:       make(chan struct{}, 1),
		sessionCancel: make(chan struct{}, 1),
	}
	conn := NewAgentConnectionFromTransport(agent, transport)
	reported := make(chan error, 1)
	conn.jsonrpcConn.OnError = func(method string, err error) {
		reported <- err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := conn.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}

	transport.inbox <- json.RawMessage(`{"jsonrpc":"2.0","method":"session/unknown","params":{"sessionId":"session-1"}}`)

	select {
	case err := <-reported:
		if err == nil || err.Error() != "unsupported notification: session/unknown" {
			t.Fatalf("reported err = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected unknown notification to be reported")
	}
}

func TestAgentSideConnectionReportsUnimplementedSessionCancel(t *testing.T) {
	transport := newChannelTransport()
	conn := NewAgentConnectionFromTransport(acp.BaseAgent{}, transport)
	reported := make(chan error, 1)
	conn.jsonrpcConn.OnError = func(method string, err error) {
		reported <- err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := conn.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}

	transport.inbox <- json.RawMessage(`{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"session-1"}}`)

	select {
	case err := <-reported:
		if err == nil || err.Error() != "notification handler not implemented: "+acp.MethodAgentSessionCancel {
			t.Fatalf("reported err = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected session/cancel to report unimplemented handler")
	}
}

// agentWireCanary is planted in the probe agent's error; the wire JSON must
// never contain it when sanitization is enabled.
const agentWireCanary = "agent-wire-canary-6e2d"

type optionProbeAgent struct {
	acp.BaseAgent
}

func (optionProbeAgent) Prompt(context.Context, acp.PromptRequest) (acp.PromptResponse, error) {
	return acp.PromptResponse{}, errors.New(agentWireCanary)
}

// captureLogger records formatted log lines so tests can assert on them.
type captureLogger struct {
	mu    sync.Mutex
	lines []string
}

func (l *captureLogger) record(format string, v ...any) {
	l.mu.Lock()
	l.lines = append(l.lines, fmt.Sprintf(format, v...))
	l.mu.Unlock()
}

func (l *captureLogger) Debug(format string, v ...any) { l.record(format, v...) }
func (l *captureLogger) Info(format string, v ...any)  { l.record(format, v...) }
func (l *captureLogger) Warn(format string, v ...any)  { l.record(format, v...) }
func (l *captureLogger) Error(format string, v ...any) { l.record(format, v...) }
func (l *captureLogger) CtxDebug(_ context.Context, format string, v ...any) {
	l.record(format, v...)
}
func (l *captureLogger) CtxInfo(_ context.Context, format string, v ...any) {
	l.record(format, v...)
}
func (l *captureLogger) CtxWarn(_ context.Context, format string, v ...any) {
	l.record(format, v...)
}
func (l *captureLogger) CtxError(_ context.Context, format string, v ...any) {
	l.record(format, v...)
}

func (l *captureLogger) contains(substr string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, line := range l.lines {
		if strings.Contains(line, substr) {
			return true
		}
	}
	return false
}

// TestAgentConnectionOptionsApplyAndSanitize drives a session/prompt frame
// through NewAgentConnectionFromTransport with every agent option set. It
// asserts the wire error is opaque (sanitization plumbed through), that the
// connection label reaches the handler-error log line, and that the
// notification-error handler is invoked.
func TestAgentConnectionOptionsApplyAndSanitize(t *testing.T) {
	prevLogger := acplog.Get()
	prevLevel := acplog.GetLevel()
	capture := &captureLogger{}
	acplog.Set(capture)
	acplog.SetLevel(acplog.LevelDebug)
	t.Cleanup(func() {
		acplog.Set(prevLogger)
		acplog.SetLevel(prevLevel)
	})

	transport := newChannelTransport()
	notifyErrors := make(chan error, 1)
	conn := NewAgentConnectionFromTransport(optionProbeAgent{}, transport,
		WithAgentMaxConsecutiveParseErrors(10),
		WithAgentRequestTimeout(2*time.Second),
		WithAgentRequestWorkers(4),
		WithAgentMaxPendingDispatch(64),
		WithAgentShutdownTimeout(2*time.Second),
		WithAgentConnectionLabel("agent-options-test"),
		WithAgentNotificationErrorHandler(func(_ string, err error) {
			notifyErrors <- err
		}),
		WithAgentSanitizedErrors(),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := conn.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = conn.Close() }()

	transport.inbox <- json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"sessionId":"session-1","prompt":[{"type":"text","text":"hi"}]}}`)

	select {
	case raw := <-transport.outbox:
		var msg struct {
			Error *acp.RPCError `json:"error"`
		}
		if err := json.Unmarshal(raw, &msg); err != nil {
			t.Fatalf("unmarshal response: %v", err)
		}
		if msg.Error == nil {
			t.Fatalf("expected error response, got %s", string(raw))
		}
		if msg.Error.Message != "internal error" {
			t.Fatalf("wire error message = %q, want %q", msg.Error.Message, "internal error")
		}
		if len(msg.Error.Data) != 0 {
			t.Fatalf("sanitized wire error carried data: %s", msg.Error.Data)
		}
		if bytes.Contains(raw, []byte(agentWireCanary)) {
			t.Fatalf("sanitized wire JSON leaks handler error: %s", raw)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for prompt response")
	}

	// The connection label must appear on the handler-error log line
	// (local logging keeps the original cause even when the wire is opaque).
	deadline := time.Now().Add(2 * time.Second)
	for !capture.contains("conn=agent-options-test") || !capture.contains(agentWireCanary) {
		if time.Now().After(deadline) {
			t.Fatal("handler-error log line missing connection label or original cause")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The notification-error handler must receive notification failures.
	transport.inbox <- json.RawMessage(`{"jsonrpc":"2.0","method":"session/unknown","params":{"sessionId":"session-1"}}`)
	select {
	case err := <-notifyErrors:
		if err == nil || !strings.Contains(err.Error(), "session/unknown") {
			t.Fatalf("notification error = %v, want unsupported notification for session/unknown", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for notification error handler")
	}
}
