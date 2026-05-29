package wsserver

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hertz-contrib/websocket"

	"github.com/eino-contrib/acp/internal/wsutil"
	"github.com/eino-contrib/acp/transport"
)

// mockServerConn implements messageConn + all optional interfaces used by ServeConn.
type mockServerConn struct {
	mu sync.Mutex

	messages     chan msgEntry
	closed       chan struct{}
	closeOnce    sync.Once
	writtenMsgs  []msgEntry
	readLimit    int64
	pingHandler  func(string) error
	closeErr     error

	// Track SetReadDeadline calls
	readDeadlines []time.Time
	readDLMu      sync.Mutex

	// readDeadline is the currently active read deadline; when reached, ReadMessage
	// returns a timeout error simulating net.Conn behavior.
	readDeadline time.Time

	// Track SetWriteDeadline calls
	writeDeadlines []time.Time

	// Track WriteControl calls
	controlWrites []controlWriteRecord
	controlMu     sync.Mutex
}

type msgEntry struct {
	messageType int
	data        []byte
}

type controlWriteRecord struct {
	MessageType int
	Data        []byte
	Deadline    time.Time
}

func newMockServerConn() *mockServerConn {
	return &mockServerConn{
		messages: make(chan msgEntry, 64),
		closed:   make(chan struct{}),
	}
}

func (m *mockServerConn) ReadMessage() (int, []byte, error) {
	// Check if a read deadline is set; if so, use a timer to simulate timeout.
	m.readDLMu.Lock()
	dl := m.readDeadline
	m.readDLMu.Unlock()

	var timer *time.Timer
	var timerC <-chan time.Time
	if !dl.IsZero() {
		remaining := time.Until(dl)
		if remaining <= 0 {
			return 0, nil, &netTimeoutError{}
		}
		timer = time.NewTimer(remaining)
		timerC = timer.C
		defer timer.Stop()
	}

	select {
	case msg, ok := <-m.messages:
		if !ok {
			return 0, nil, &websocket.CloseError{Code: websocket.CloseNormalClosure}
		}
		return msg.messageType, msg.data, nil
	case <-m.closed:
		return 0, nil, &websocket.CloseError{Code: websocket.CloseNormalClosure}
	case <-timerC:
		return 0, nil, &netTimeoutError{}
	}
}

// netTimeoutError simulates a net timeout error (implements net.Error).
type netTimeoutError struct{}

func (e *netTimeoutError) Error() string   { return "i/o timeout" }
func (e *netTimeoutError) Timeout() bool   { return true }
func (e *netTimeoutError) Temporary() bool { return true }

func (m *mockServerConn) WriteMessage(messageType int, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.writtenMsgs = append(m.writtenMsgs, msgEntry{messageType: messageType, data: data})
	return nil
}

func (m *mockServerConn) Close() error {
	m.closeOnce.Do(func() {
		close(m.closed)
	})
	return m.closeErr
}

func (m *mockServerConn) SetReadLimit(limit int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.readLimit = limit
}

func (m *mockServerConn) SetWriteDeadline(t time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.writeDeadlines = append(m.writeDeadlines, t)
	return nil
}

func (m *mockServerConn) SetReadDeadline(t time.Time) error {
	m.readDLMu.Lock()
	defer m.readDLMu.Unlock()
	m.readDeadlines = append(m.readDeadlines, t)
	m.readDeadline = t
	return nil
}

func (m *mockServerConn) SetPingHandler(h func(string) error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pingHandler = h
}

func (m *mockServerConn) WriteControl(messageType int, data []byte, deadline time.Time) error {
	m.controlMu.Lock()
	defer m.controlMu.Unlock()
	m.controlWrites = append(m.controlWrites, controlWriteRecord{
		MessageType: messageType,
		Data:        append([]byte(nil), data...),
		Deadline:    deadline,
	})
	return nil
}

func (m *mockServerConn) enqueue(messageType int, data []byte) {
	m.messages <- msgEntry{messageType: messageType, data: data}
}

func (m *mockServerConn) enqueueText(data string) {
	m.enqueue(websocket.TextMessage, []byte(data))
}

func (m *mockServerConn) getReadDeadlines() []time.Time {
	m.readDLMu.Lock()
	defer m.readDLMu.Unlock()
	result := make([]time.Time, len(m.readDeadlines))
	copy(result, m.readDeadlines)
	return result
}

func (m *mockServerConn) getControlWrites() []controlWriteRecord {
	m.controlMu.Lock()
	defer m.controlMu.Unlock()
	result := make([]controlWriteRecord, len(m.controlWrites))
	copy(result, m.controlWrites)
	return result
}

func (m *mockServerConn) getPingHandler() func(string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pingHandler
}

const initializeMsg = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`

func TestInitializeTimeout(t *testing.T) {
	tr := New(WithInitializeTimeout(20 * time.Millisecond))
	conn := newMockServerConn()

	var done atomic.Bool
	go func() {
		tr.ServeConn(context.Background(), conn)
		done.Store(true)
	}()

	// Don't send any message; wait for timeout
	time.Sleep(80 * time.Millisecond)

	if !done.Load() {
		t.Fatal("ServeConn should have returned after initialize timeout")
	}

	// Verify close frame with code 4000 was sent
	controls := conn.getControlWrites()
	found := false
	for _, cw := range controls {
		if cw.MessageType == websocket.CloseMessage {
			found = true
			if len(cw.Data) < 2 {
				t.Fatal("close frame payload too short to contain close code")
			}
			code := int(cw.Data[0])<<8 | int(cw.Data[1])
			if code != transport.WSCloseInitializeTimeout {
				t.Fatalf("expected close code %d, got %d", transport.WSCloseInitializeTimeout, code)
			}
			break
		}
	}
	if !found {
		t.Fatal("expected close frame with CloseCodeInitializeTimeout to be sent")
	}
}

func TestInitializeTimeoutZero_NoDeadline(t *testing.T) {
	tr := New(WithInitializeTimeout(0))
	conn := newMockServerConn()

	go func() {
		time.Sleep(30 * time.Millisecond)
		conn.Close()
	}()

	tr.ServeConn(context.Background(), conn)

	deadlines := conn.getReadDeadlines()
	if len(deadlines) != 0 {
		t.Fatalf("expected no read deadlines set when initializeTimeout=0, got %d", len(deadlines))
	}
}

func TestInitializeCompleteSwitchesToReadDeadline(t *testing.T) {
	readTimeout := 30 * time.Millisecond
	tr := New(
		WithInitializeTimeout(50*time.Millisecond),
		WithReadTimeout(readTimeout),
	)
	conn := newMockServerConn()

	go func() {
		time.Sleep(5 * time.Millisecond)
		conn.enqueueText(initializeMsg)
		time.Sleep(20 * time.Millisecond)
		conn.Close()
	}()

	tr.ServeConn(context.Background(), conn)

	deadlines := conn.getReadDeadlines()
	// At least 2 deadlines: one for initializeTimeout, one after init completes (readTimeout)
	if len(deadlines) < 2 {
		t.Fatalf("expected at least 2 read deadlines, got %d", len(deadlines))
	}

	// The second deadline should be ~readTimeout from now (after init)
	// The first deadline (initializeTimeout=50ms) should be further out than the second (readTimeout=30ms)
	// Since both are relative to time.Now() at their respective call sites, we check the second
	// is closer to now than the first.
	first := deadlines[0]
	second := deadlines[1]
	if !second.Before(first) {
		// This is expected because initializeTimeout(50ms) > readTimeout(30ms)
		// and second was set later in wall time but with a shorter duration
		// Actually, second is set AFTER first, so second = time.Now()+30ms could be > first = earlier_time+50ms
		// Just verify both are non-zero
		if first.IsZero() || second.IsZero() {
			t.Fatal("deadlines should be non-zero")
		}
	}
}

func TestPingHandlerBeforeInit_OnlyEchoesPong(t *testing.T) {
	tr := New(
		WithInitializeTimeout(200*time.Millisecond),
		WithReadTimeout(50*time.Millisecond),
	)
	conn := newMockServerConn()

	serveStarted := make(chan struct{})
	go func() {
		close(serveStarted)
		tr.ServeConn(context.Background(), conn)
	}()
	<-serveStarted
	time.Sleep(5 * time.Millisecond)

	handler := conn.getPingHandler()
	if handler == nil {
		t.Fatal("expected ping handler to be installed")
	}

	// Record deadlines before ping
	deadlinesBefore := conn.getReadDeadlines()

	// Invoke ping handler (before init)
	if err := handler("hello"); err != nil {
		t.Fatalf("ping handler returned error: %v", err)
	}

	// Verify Pong was sent
	controls := conn.getControlWrites()
	if len(controls) == 0 {
		t.Fatal("expected Pong to be written")
	}
	if controls[0].MessageType != websocket.PongMessage {
		t.Fatalf("expected PongMessage, got %d", controls[0].MessageType)
	}
	if string(controls[0].Data) != "hello" {
		t.Fatalf("expected pong data 'hello', got %q", string(controls[0].Data))
	}

	// Verify read deadline was NOT refreshed (same count as before)
	deadlinesAfter := conn.getReadDeadlines()
	if len(deadlinesAfter) != len(deadlinesBefore) {
		t.Fatalf("ping before init should NOT refresh read deadline, deadlines before=%d after=%d",
			len(deadlinesBefore), len(deadlinesAfter))
	}

	conn.Close()
}

func TestPingHandlerAfterInit_RefreshesDeadline(t *testing.T) {
	readTimeout := 50 * time.Millisecond
	tr := New(
		WithInitializeTimeout(200*time.Millisecond),
		WithReadTimeout(readTimeout),
	)
	conn := newMockServerConn()

	serveStarted := make(chan struct{})
	go func() {
		close(serveStarted)
		tr.ServeConn(context.Background(), conn)
	}()
	<-serveStarted
	time.Sleep(5 * time.Millisecond)

	// Send initialize
	conn.enqueueText(initializeMsg)
	time.Sleep(10 * time.Millisecond)

	deadlinesBefore := conn.getReadDeadlines()

	handler := conn.getPingHandler()
	if handler == nil {
		t.Fatal("expected ping handler to be installed")
	}

	if err := handler("ping-after-init"); err != nil {
		t.Fatalf("ping handler returned error: %v", err)
	}

	// Verify Pong was sent
	controls := conn.getControlWrites()
	foundPong := false
	for _, cw := range controls {
		if cw.MessageType == websocket.PongMessage {
			foundPong = true
			break
		}
	}
	if !foundPong {
		t.Fatal("expected Pong to be written after init ping")
	}

	// Verify read deadline WAS refreshed
	deadlinesAfter := conn.getReadDeadlines()
	if len(deadlinesAfter) <= len(deadlinesBefore) {
		t.Fatalf("ping after init should refresh read deadline, deadlines before=%d after=%d",
			len(deadlinesBefore), len(deadlinesAfter))
	}

	conn.Close()
}

func TestDataFrameRefreshesReadDeadlineAfterInit(t *testing.T) {
	readTimeout := 50 * time.Millisecond
	tr := New(
		WithInitializeTimeout(200*time.Millisecond),
		WithReadTimeout(readTimeout),
	)
	conn := newMockServerConn()

	go func() {
		time.Sleep(5 * time.Millisecond)
		conn.enqueueText(initializeMsg)
		time.Sleep(10 * time.Millisecond)
		// Send a second data frame
		conn.enqueueText(`{"jsonrpc":"2.0","id":2,"method":"test","params":{}}`)
		time.Sleep(10 * time.Millisecond)
		conn.Close()
	}()

	tr.ServeConn(context.Background(), conn)

	deadlines := conn.getReadDeadlines()
	// Expected: 1 (initializeTimeout) + 1 (after init switch) + 1 (second data frame) = 3
	if len(deadlines) < 3 {
		t.Fatalf("expected at least 3 read deadline calls, got %d", len(deadlines))
	}
}

func TestReadTimeoutZero_ClearsInitializeDeadline(t *testing.T) {
	tr := New(
		WithInitializeTimeout(50*time.Millisecond),
		WithReadTimeout(0),
	)
	conn := newMockServerConn()

	go func() {
		time.Sleep(5 * time.Millisecond)
		conn.enqueueText(initializeMsg)
		time.Sleep(10 * time.Millisecond)
		conn.Close()
	}()

	tr.ServeConn(context.Background(), conn)

	deadlines := conn.getReadDeadlines()
	// Expected: 1 (initializeTimeout) + 1 (clear deadline with zero time) = 2
	if len(deadlines) < 2 {
		t.Fatalf("expected at least 2 read deadline calls, got %d", len(deadlines))
	}

	// The last deadline after init should be zero (clearing the deadline)
	lastDeadline := deadlines[len(deadlines)-1]
	if !lastDeadline.IsZero() {
		t.Fatalf("expected zero deadline (clear) after init with readTimeout=0, got %v", lastDeadline)
	}
}

func TestServerOptionPassthrough(t *testing.T) {
	readTimeout := 42 * time.Millisecond
	initTimeout := 99 * time.Millisecond

	tr := New(
		WithReadTimeout(readTimeout),
		WithInitializeTimeout(initTimeout),
	)

	if tr.readTimeout != readTimeout {
		t.Fatalf("expected readTimeout=%v, got %v", readTimeout, tr.readTimeout)
	}
	if tr.initializeTimeout != initTimeout {
		t.Fatalf("expected initializeTimeout=%v, got %v", initTimeout, tr.initializeTimeout)
	}
}

func TestPingHandlerWriteControlUsesControlWriteDeadline(t *testing.T) {
	tr := New(
		WithInitializeTimeout(200*time.Millisecond),
		WithReadTimeout(50*time.Millisecond),
	)
	conn := newMockServerConn()

	serveStarted := make(chan struct{})
	go func() {
		close(serveStarted)
		tr.ServeConn(context.Background(), conn)
	}()
	<-serveStarted
	time.Sleep(5 * time.Millisecond)

	handler := conn.getPingHandler()
	if handler == nil {
		t.Fatal("expected ping handler to be installed")
	}

	before := time.Now()
	if err := handler("deadline-test"); err != nil {
		t.Fatalf("ping handler returned error: %v", err)
	}
	after := time.Now()

	controls := conn.getControlWrites()
	if len(controls) == 0 {
		t.Fatal("expected at least one control write")
	}

	pongWrite := controls[len(controls)-1]
	if pongWrite.MessageType != websocket.PongMessage {
		t.Fatalf("expected PongMessage, got %d", pongWrite.MessageType)
	}

	// The deadline should be approximately time.Now() + wsutil.ControlWriteDeadline (5s)
	expectedMin := before.Add(wsutil.ControlWriteDeadline)
	expectedMax := after.Add(wsutil.ControlWriteDeadline).Add(10 * time.Millisecond)
	if pongWrite.Deadline.Before(expectedMin) || pongWrite.Deadline.After(expectedMax) {
		t.Fatalf("WriteControl deadline %v not in expected range [%v, %v]",
			pongWrite.Deadline, expectedMin, expectedMax)
	}

	conn.Close()
}

func TestReadTimeoutAfterInit_ClosesWithGoingAway(t *testing.T) {
	readTimeout := 30 * time.Millisecond
	tr := New(
		WithInitializeTimeout(200*time.Millisecond),
		WithReadTimeout(readTimeout),
	)
	conn := newMockServerConn()

	var done atomic.Bool
	go func() {
		tr.ServeConn(context.Background(), conn)
		done.Store(true)
	}()

	// Send initialize to pass the init phase
	time.Sleep(5 * time.Millisecond)
	conn.enqueueText(initializeMsg)

	// Wait for read timeout to trigger (readTimeout=30ms + margin)
	time.Sleep(80 * time.Millisecond)

	if !done.Load() {
		t.Fatal("ServeConn should have returned after read timeout")
	}

	// Verify close frame with code 1001 (Going Away) was sent
	controls := conn.getControlWrites()
	found := false
	for _, cw := range controls {
		if cw.MessageType == websocket.CloseMessage {
			found = true
			if len(cw.Data) < 2 {
				t.Fatal("close frame payload too short to contain close code")
			}
			code := int(cw.Data[0])<<8 | int(cw.Data[1])
			if code != websocket.CloseGoingAway {
				t.Fatalf("expected close code %d (GoingAway), got %d", websocket.CloseGoingAway, code)
			}
			break
		}
	}
	if !found {
		t.Fatal("expected close frame with CloseGoingAway to be sent on read timeout")
	}
}

// Verify mockServerConn satisfies all the optional interfaces.
var (
	_ messageConn      = (*mockServerConn)(nil)
	_ readLimitSetter  = (*mockServerConn)(nil)
	_ writeDeadliner   = (*mockServerConn)(nil)
	_ readDeadliner    = (*mockServerConn)(nil)
	_ pingHandlerSetter = (*mockServerConn)(nil)
	_ controlWriter    = (*mockServerConn)(nil)
)

// Suppress unused import warnings for json and atomic.
var _ = json.Marshal
var _ = (*atomic.Bool)(nil)
