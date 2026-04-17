package jsonrpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	acp "github.com/eino-contrib/acp"
	acplog "github.com/eino-contrib/acp/internal/log"
	"github.com/eino-contrib/acp/internal/safe"
	"github.com/eino-contrib/acp/transport"
)

type blockingTransport struct {
	closed chan struct{}
	once   sync.Once
}

func newBlockingTransport() *blockingTransport {
	return &blockingTransport{closed: make(chan struct{})}
}

func (t *blockingTransport) ReadMessage(context.Context) (json.RawMessage, error) {
	<-t.closed
	return nil, io.EOF
}

func (t *blockingTransport) WriteMessage(context.Context, json.RawMessage) error {
	return nil
}

func (t *blockingTransport) Close() error {
	t.once.Do(func() {
		close(t.closed)
	})
	return nil
}

func TestStartReturnsOnContextCancel(t *testing.T) {
	transport := newBlockingTransport()
	conn := NewConnection(transport, nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	safe.GoWithLogger(acplog.Default(), func() {
		done <- conn.Start(ctx)
	})

	if err := conn.WaitUntilStarted(context.Background()); err != nil {
		t.Fatalf("wait until started: %v", err)
	}

	cancel()

	select {
	case err := <-done:
		if err == nil || err != context.Canceled {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return after context cancellation")
	}
}

type errorTransport struct {
	err error
}

func (t *errorTransport) ReadMessage(context.Context) (json.RawMessage, error) {
	return nil, t.err
}

func (t *errorTransport) WriteMessage(context.Context, json.RawMessage) error {
	return nil
}

func (t *errorTransport) Close() error {
	return nil
}

type blockingWriteTransport struct {
	closed chan struct{}
	once   sync.Once
}

func newBlockingWriteTransport() *blockingWriteTransport {
	return &blockingWriteTransport{closed: make(chan struct{})}
}

func (t *blockingWriteTransport) ReadMessage(context.Context) (json.RawMessage, error) {
	<-t.closed
	return nil, io.EOF
}

func (t *blockingWriteTransport) WriteMessage(ctx context.Context, _ json.RawMessage) error {
	<-ctx.Done()
	return ctx.Err()
}

func (t *blockingWriteTransport) Close() error {
	t.once.Do(func() {
		close(t.closed)
	})
	return nil
}

func TestStartReturnsTerminalTransportError(t *testing.T) {
	transport := &errorTransport{err: errors.New("boom")}
	conn := NewConnection(transport, nil, nil)

	err := conn.Start(context.Background())
	if err == nil || err.Error() != "boom" {
		t.Fatalf("expected terminal error boom, got %v", err)
	}
	if got := conn.TerminalError(); got == nil || got.Error() != "boom" {
		t.Fatalf("TerminalError = %v, want boom", got)
	}
}

func TestSendRequestBeforeStartReturnsSentinelNotStarted(t *testing.T) {
	conn := NewConnection(newBlockingTransport(), nil, nil)
	_, err := conn.SendRequest(context.Background(), "ping", nil)
	if err == nil || !errors.Is(err, transport.ErrConnNotStarted) {
		t.Fatalf("SendRequest err = %v, want errors.Is(ErrConnNotStarted)", err)
	}
}

func TestSendNotificationBeforeStartReturnsSentinelNotStarted(t *testing.T) {
	conn := NewConnection(newBlockingTransport(), nil, nil)
	err := conn.SendNotification(context.Background(), "ping", nil)
	if err == nil || !errors.Is(err, transport.ErrConnNotStarted) {
		t.Fatalf("SendNotification err = %v, want errors.Is(ErrConnNotStarted)", err)
	}
}

func TestToResponseErrorPreservesOriginErrorInData(t *testing.T) {
	rpcErr := ToResponseError(errors.New("provider upstream 502"))
	if rpcErr == nil {
		t.Fatal("expected rpcErr")
	}
	if rpcErr.Code != int(acp.ErrorCodeInternalError) {
		t.Fatalf("rpcErr.Code = %d, want %d", rpcErr.Code, acp.ErrorCodeInternalError)
	}
	if rpcErr.Message != genericInternalErrorMessage {
		t.Fatalf("rpcErr.Message = %q, want %q", rpcErr.Message, genericInternalErrorMessage)
	}

	var payload struct {
		Error       string `json:"error"`
		OriginError string `json:"originError"`
	}
	if err := json.Unmarshal(rpcErr.Data, &payload); err != nil {
		t.Fatalf("unmarshal rpcErr.Data: %v", err)
	}
	if payload.Error != "internal error" {
		t.Fatalf("payload.Error = %q, want %q", payload.Error, "internal error")
	}
	if payload.OriginError != "provider upstream 502" {
		t.Fatalf("payload.OriginError = %q, want %q", payload.OriginError, "provider upstream 502")
	}
}

func TestSendNotificationUsesCallerContextForWrite(t *testing.T) {
	transport := newBlockingWriteTransport()
	conn := NewConnection(transport, nil, nil)

	connCtx, cancelConn := context.WithCancel(context.Background())
	defer cancelConn()

	done := make(chan error, 1)
	safe.GoWithLogger(acplog.Default(), func() {
		done <- conn.Start(connCtx)
	})

	if err := conn.WaitUntilStarted(context.Background()); err != nil {
		t.Fatalf("wait until started: %v", err)
	}

	writeCtx, cancelWrite := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelWrite()

	err := conn.SendNotification(writeCtx, "test/notify", map[string]string{"ok": "1"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("SendNotification error = %v, want %v", err, context.DeadlineExceeded)
	}

	cancelConn()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("connection did not stop after cancellation")
	}
}

// channelTransport is a test transport that delivers messages from a channel.
type channelTransport struct {
	inbox  chan json.RawMessage
	outbox chan json.RawMessage
	done   chan struct{}
	once   sync.Once
}

func newChannelTransport() *channelTransport {
	return &channelTransport{
		inbox:  make(chan json.RawMessage, 64),
		outbox: make(chan json.RawMessage, 64),
		done:   make(chan struct{}),
	}
}

func (t *channelTransport) ReadMessage(ctx context.Context) (json.RawMessage, error) {
	select {
	case msg, ok := <-t.inbox:
		if !ok {
			return nil, io.EOF
		}
		return msg, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-t.done:
		return nil, io.EOF
	}
}

func (t *channelTransport) WriteMessage(_ context.Context, data json.RawMessage) error {
	select {
	case t.outbox <- data:
		return nil
	case <-t.done:
		return errors.New("closed")
	}
}

func (t *channelTransport) Close() error {
	t.once.Do(func() { close(t.done) })
	return nil
}

func TestNotificationsProcessedInOrder(t *testing.T) {
	transport := newChannelTransport()
	var mu sync.Mutex
	var received []int

	handler := func(_ context.Context, method string, _ json.RawMessage) (any, error) {
		return nil, nil
	}
	notifHandler := func(_ context.Context, method string, params json.RawMessage) error {
		// Simulate some work to increase the chance of reordering
		// if notifications were dispatched concurrently.
		time.Sleep(time.Millisecond)
		var payload struct {
			Index int `json:"index"`
		}
		if err := json.Unmarshal(params, &payload); err != nil {
			return err
		}
		mu.Lock()
		received = append(received, payload.Index)
		mu.Unlock()
		return nil
	}

	conn := NewConnection(transport, handler, notifHandler, WithOrderedNotificationMatcher(func(method string) bool {
		return method == "session/update"
	}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	safe.GoWithLogger(acplog.Default(), func() {
		if err := conn.Start(ctx); err != nil {
			acplog.Default().Error("start ordered notification test connection: %v", err)
		}
	})
	if err := conn.WaitUntilStarted(ctx); err != nil {
		t.Fatalf("wait: %v", err)
	}

	// Send 50 notifications in order.
	const count = 50
	for i := 0; i < count; i++ {
		msg := json.RawMessage(fmt.Sprintf(`{"jsonrpc":"2.0","method":"session/update","params":{"index":%d}}`, i))
		transport.inbox <- msg
	}

	// Wait for all to be processed.
	deadline := time.After(5 * time.Second)
	for {
		mu.Lock()
		n := len(received)
		mu.Unlock()
		if n >= count {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for notifications, got %d/%d", n, count)
		case <-time.After(10 * time.Millisecond):
		}
	}

	// Verify order is preserved.
	mu.Lock()
	defer mu.Unlock()
	for i := 0; i < count; i++ {
		if received[i] != i {
			t.Fatalf("notification %d: got %d, want %d", i, received[i], i)
		}
	}
}

func TestSessionUpdateNotificationsAreUnorderedByDefault(t *testing.T) {
	transport := newChannelTransport()
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondHandled := make(chan struct{})

	conn := NewConnection(transport, nil, func(_ context.Context, method string, params json.RawMessage) error {
		var payload struct {
			Index int `json:"index"`
		}
		if err := json.Unmarshal(params, &payload); err != nil {
			return err
		}
		switch payload.Index {
		case 1:
			close(firstStarted)
			<-releaseFirst
		case 2:
			close(secondHandled)
		}
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	safe.GoWithLogger(acplog.Default(), func() {
		done <- conn.Start(ctx)
	})
	if err := conn.WaitUntilStarted(ctx); err != nil {
		t.Fatalf("wait: %v", err)
	}

	transport.inbox <- json.RawMessage(`{"jsonrpc":"2.0","method":"session/update","params":{"index":1}}`)
	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first notification did not start")
	}

	transport.inbox <- json.RawMessage(`{"jsonrpc":"2.0","method":"session/update","params":{"index":2}}`)
	select {
	case <-secondHandled:
	case <-time.After(time.Second):
		t.Fatal("second notification was blocked by default ordering")
	}

	close(releaseFirst)

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Start error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("connection did not stop after cancellation")
	}
}

func TestStartReportsNullIDErrorResponse(t *testing.T) {
	transport := newChannelTransport()
	conn := NewConnection(transport, nil, nil)

	errCh := make(chan error, 1)
	conn.OnError = func(_ string, err error) {
		select {
		case errCh <- err:
		default:
		}
	}

	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	safe.GoWithLogger(acplog.Default(), func() {
		done <- conn.Start(ctx)
	})
	if err := conn.WaitUntilStarted(ctx); err != nil {
		t.Fatalf("wait until started: %v", err)
	}

	transport.inbox <- json.RawMessage(`{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"parse error"}}`)
	close(transport.inbox)

	select {
	case err := <-errCh:
		var rpcErr *acp.RPCError
		if !errors.As(err, &rpcErr) {
			t.Fatalf("OnError err = %T %v, want *acp.RPCError", err, err)
		}
		if rpcErr.Code != int(acp.ErrorCodeParseError) {
			t.Fatalf("OnError code = %d, want %d", rpcErr.Code, int(acp.ErrorCodeParseError))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for null-id response error")
	}

	select {
	case err := <-done:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("Start err = %v, want EOF", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for connection shutdown")
	}
}

func TestQueuedNotificationsDrainWithoutCanceledContext(t *testing.T) {
	transport := newChannelTransport()
	firstStarted := make(chan struct{})
	unblockFirst := make(chan struct{})
	secondHandled := make(chan error, 1)

	conn := NewConnection(transport, nil, func(ctx context.Context, method string, _ json.RawMessage) error {
		switch method {
		case "test/first":
			close(firstStarted)
			<-unblockFirst
			return ctx.Err()
		case "test/queued":
			secondHandled <- ctx.Err()
			return nil
		default:
			return nil
		}
	}, WithOrderedNotificationMatcher(func(method string) bool {
		return method == "test/first" || method == "test/queued"
	}))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	safe.GoWithLogger(acplog.Default(), func() {
		done <- conn.Start(ctx)
	})
	if err := conn.WaitUntilStarted(ctx); err != nil {
		t.Fatalf("wait: %v", err)
	}

	transport.inbox <- json.RawMessage(`{"jsonrpc":"2.0","method":"test/first"}`)
	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first notification did not start")
	}

	transport.inbox <- json.RawMessage(`{"jsonrpc":"2.0","method":"test/queued"}`)
	deadline := time.After(time.Second)
	for conn.orderedNotificationQueue.Len() == 0 {
		select {
		case <-deadline:
			t.Fatal("queued notification was not buffered before cancellation")
		case <-time.After(10 * time.Millisecond):
		}
	}

	cancel()
	close(unblockFirst)

	select {
	case err := <-secondHandled:
		if err != nil {
			t.Fatalf("queued notification saw canceled context: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queued notification was not drained after cancellation")
	}

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Start error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("connection did not stop after cancellation")
	}
}

func TestUnorderedNotificationsDoNotBlockEachOther(t *testing.T) {
	transport := newChannelTransport()
	slowStarted := make(chan struct{})
	releaseSlow := make(chan struct{})
	fastHandled := make(chan struct{})

	conn := NewConnection(transport, nil, func(_ context.Context, method string, _ json.RawMessage) error {
		switch method {
		case "test/slow":
			close(slowStarted)
			<-releaseSlow
		case "test/fast":
			close(fastHandled)
		}
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	safe.GoWithLogger(acplog.Default(), func() {
		done <- conn.Start(ctx)
	})
	if err := conn.WaitUntilStarted(ctx); err != nil {
		t.Fatalf("wait: %v", err)
	}

	transport.inbox <- json.RawMessage(`{"jsonrpc":"2.0","method":"test/slow"}`)
	select {
	case <-slowStarted:
	case <-time.After(time.Second):
		t.Fatal("slow notification did not start")
	}

	transport.inbox <- json.RawMessage(`{"jsonrpc":"2.0","method":"test/fast"}`)
	select {
	case <-fastHandled:
	case <-time.After(time.Second):
		t.Fatal("fast notification was blocked behind slow notification")
	}

	close(releaseSlow)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("connection did not stop after cancellation")
	}
}

func TestRequestHandlerPanicReturnsInternalErrorResponse(t *testing.T) {
	transport := newChannelTransport()
	conn := NewConnection(transport, func(context.Context, string, json.RawMessage) (any, error) {
		panic("boom")
	}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	safe.GoWithLogger(acplog.Default(), func() {
		done <- conn.Start(ctx)
	})
	if err := conn.WaitUntilStarted(ctx); err != nil {
		t.Fatalf("wait: %v", err)
	}

	transport.inbox <- json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"test/panic","params":{}}`)

	select {
	case raw := <-transport.outbox:
		var msg Message
		if err := json.Unmarshal(raw, &msg); err != nil {
			t.Fatalf("unmarshal panic response: %v", err)
		}
		if msg.Error == nil {
			t.Fatalf("expected panic error response, got %s", string(raw))
		}
		if msg.Error.Code != int(acp.ErrorCodeInternalError) {
			t.Fatalf("error code = %d, want %d", msg.Error.Code, int(acp.ErrorCodeInternalError))
		}
		if msg.Error.Message != genericInternalErrorMessage {
			t.Fatalf("error message = %q, want %q", msg.Error.Message, genericInternalErrorMessage)
		}
		if msg.ID == nil || msg.ID.String() != "n:1" {
			t.Fatalf("response id = %v, want n:1", msg.ID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for panic response")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("connection did not stop after cancellation")
	}
}

func TestRequestHandlerInternalErrorDoesNotLeakDetails(t *testing.T) {
	transport := newChannelTransport()
	conn := NewConnection(transport, func(context.Context, string, json.RawMessage) (any, error) {
		return nil, errors.New("sensitive backend detail")
	}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = conn.Start(ctx) }()
	if err := conn.WaitUntilStarted(ctx); err != nil {
		t.Fatalf("wait: %v", err)
	}

	transport.inbox <- json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"test/error","params":{}}`)

	select {
	case raw := <-transport.outbox:
		var msg Message
		if err := json.Unmarshal(raw, &msg); err != nil {
			t.Fatalf("unmarshal error response: %v", err)
		}
		if msg.Error == nil {
			t.Fatalf("expected error response, got %s", string(raw))
		}
		if msg.Error.Message != genericInternalErrorMessage {
			t.Fatalf("error message = %q, want %q", msg.Error.Message, genericInternalErrorMessage)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for error response")
	}
}

func TestNotificationHandlerPanicDoesNotStopProcessing(t *testing.T) {
	transport := newChannelTransport()
	processed := make(chan string, 1)
	reported := make(chan string, 1)

	conn := NewConnection(transport, nil, func(_ context.Context, method string, _ json.RawMessage) error {
		if method == "test/panic" {
			panic("boom")
		}
		processed <- method
		return nil
	})
	conn.OnError = func(method string, err error) {
		reported <- method
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	safe.GoWithLogger(acplog.Default(), func() {
		done <- conn.Start(ctx)
	})
	if err := conn.WaitUntilStarted(ctx); err != nil {
		t.Fatalf("wait: %v", err)
	}

	transport.inbox <- json.RawMessage(`{"jsonrpc":"2.0","method":"test/panic"}`)
	transport.inbox <- json.RawMessage(`{"jsonrpc":"2.0","method":"test/after"}`)

	select {
	case method := <-reported:
		if method != "test/panic" {
			t.Fatalf("reported method = %q, want test/panic", method)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for panic to be reported")
	}

	select {
	case method := <-processed:
		if method != "test/after" {
			t.Fatalf("processed method = %q, want test/after", method)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for notification processing to continue")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("connection did not stop after cancellation")
	}
}
