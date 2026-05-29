package ws

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hertz-contrib/websocket"
)

// netTimeoutErr implements net.Error with Timeout() == true for testing
// the pong timeout classification branch in readLoop.
type netTimeoutErr struct{}

func (netTimeoutErr) Error() string   { return "i/o timeout" }
func (netTimeoutErr) Timeout() bool   { return true }
func (netTimeoutErr) Temporary() bool { return false }

// mockHeartbeatConn implements websocketConn for heartbeat testing.
type mockHeartbeatConn struct {
	mu sync.Mutex

	pingSent      []time.Time // timestamps of Ping WriteControl calls
	pingDeadlines []time.Time // deadlines passed to Ping WriteControl calls

	pongHandler func(string) error

	readDeadlines  []time.Time
	writeDeadlines []time.Time

	closeCalled atomic.Int32

	// readCh controls ReadMessage behavior. Send a readResult to unblock.
	// Close readCh or close closeCh to make ReadMessage return an error.
	readCh  chan readResult
	closeCh chan struct{}

	// writeControlErr, if non-nil, is returned by WriteControl.
	writeControlErr error
}

type readResult struct {
	msgType int
	data    []byte
	err     error
}

func newMockHeartbeatConn() *mockHeartbeatConn {
	return &mockHeartbeatConn{
		readCh:  make(chan readResult, 16),
		closeCh: make(chan struct{}),
	}
}

func (m *mockHeartbeatConn) ReadMessage() (int, []byte, error) {
	select {
	case r, ok := <-m.readCh:
		if !ok {
			return 0, nil, errors.New("connection closed")
		}
		return r.msgType, r.data, r.err
	case <-m.closeCh:
		return 0, nil, errors.New("connection closed")
	}
}

func (m *mockHeartbeatConn) WriteMessage(msgType int, data []byte) error {
	return nil
}

func (m *mockHeartbeatConn) SetWriteDeadline(t time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.writeDeadlines = append(m.writeDeadlines, t)
	return nil
}

func (m *mockHeartbeatConn) SetReadDeadline(t time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.readDeadlines = append(m.readDeadlines, t)
	return nil
}

func (m *mockHeartbeatConn) SetReadLimit(int64) {}

func (m *mockHeartbeatConn) SetPongHandler(h func(appData string) error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pongHandler = h
}

func (m *mockHeartbeatConn) WriteControl(messageType int, data []byte, deadline time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if messageType == websocket.PingMessage {
		m.pingSent = append(m.pingSent, time.Now())
		m.pingDeadlines = append(m.pingDeadlines, deadline)
	}
	if m.writeControlErr != nil {
		return m.writeControlErr
	}
	return nil
}

func (m *mockHeartbeatConn) Close() error {
	m.closeCalled.Add(1)
	// Signal closeCh only once.
	select {
	case <-m.closeCh:
	default:
		close(m.closeCh)
	}
	return nil
}

func (m *mockHeartbeatConn) getPongHandler() func(string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pongHandler
}

func (m *mockHeartbeatConn) getPingCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.pingSent)
}

func (m *mockHeartbeatConn) getReadDeadlineCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.readDeadlines)
}

func (m *mockHeartbeatConn) getPingDeadlines() []time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]time.Time, len(m.pingDeadlines))
	copy(cp, m.pingDeadlines)
	return cp
}

func (m *mockHeartbeatConn) getReadDeadlines() []time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]time.Time, len(m.readDeadlines))
	copy(cp, m.readDeadlines)
	return cp
}

// newTestTransport creates a minimal WebSocketClientTransport for heartbeat tests.
func newTestTransport(pingInterval, readTimeout time.Duration) *WebSocketClientTransport {
	t := &WebSocketClientTransport{
		pingInterval: pingInterval,
		readTimeout:  readTimeout,
		inbox:        make(chan json.RawMessage, 16),
		done:         make(chan struct{}),
		writePermit:  make(chan struct{}, 1),
	}
	t.writePermit <- struct{}{}
	return t
}

// TestClientPeriodicPing verifies Ping frames are sent at the configured interval.
func TestClientPeriodicPing(t *testing.T) {
	conn := newMockHeartbeatConn()
	tr := newTestTransport(15*time.Millisecond, 50*time.Millisecond)
	tr.wsConn = conn
	tr.connected = true
	tr.readDone = make(chan struct{})

	tr.installHeartbeat(conn)

	// Wait enough time for at least 3 pings.
	time.Sleep(55 * time.Millisecond)

	// Close to stop pingPump.
	tr.closeDone()
	conn.Close()

	if tr.pingDone != nil {
		<-tr.pingDone
	}

	count := conn.getPingCount()
	if count < 3 {
		t.Errorf("expected at least 3 pings, got %d", count)
	}
}

// TestPongHandlerRefreshesReadDeadline verifies calling pongHandler updates SetReadDeadline.
func TestPongHandlerRefreshesReadDeadline(t *testing.T) {
	conn := newMockHeartbeatConn()
	tr := newTestTransport(0, 30*time.Millisecond)

	tr.installHeartbeat(conn)

	handler := conn.getPongHandler()
	if handler == nil {
		t.Fatal("pongHandler was not set")
	}

	initialCount := conn.getReadDeadlineCount()

	if err := handler(""); err != nil {
		t.Fatalf("pongHandler returned error: %v", err)
	}

	afterCount := conn.getReadDeadlineCount()
	if afterCount <= initialCount {
		t.Errorf("expected SetReadDeadline count to increase; before=%d after=%d", initialCount, afterCount)
	}
}

// TestPongTimeoutTerminalError verifies that when ReadMessage blocks past the
// pong timeout, the transport sets a terminal error.
func TestPongTimeoutTerminalError(t *testing.T) {
	conn := newMockHeartbeatConn()
	tr := newTestTransport(0, 20*time.Millisecond)
	tr.wsConn = conn
	tr.connected = true
	tr.readDone = make(chan struct{})

	tr.installHeartbeat(conn)

	// Start readLoop manually.
	go func() {
		defer close(tr.readDone)
		defer tr.closeDone()
		tr.readLoop()
	}()

	// Simulate read deadline expiry by sending a net.Error with Timeout()=true,
	// which exercises the pong_timeout classification branch in readLoop.
	time.Sleep(25 * time.Millisecond)
	conn.readCh <- readResult{err: netTimeoutErr{}}

	select {
	case <-tr.readDone:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("readLoop did not exit after read error")
	}

	if err := tr.getTerminalError(); err == nil {
		t.Error("expected terminal error to be set")
	}
}

// TestPingWriteFailureConvergence verifies that a WriteControl failure sets
// termErr, closes conn, and readLoop exits.
func TestPingWriteFailureConvergence(t *testing.T) {
	conn := newMockHeartbeatConn()
	conn.writeControlErr = errors.New("write failed")

	tr := newTestTransport(10*time.Millisecond, 50*time.Millisecond)
	tr.wsConn = conn
	tr.connected = true
	tr.readDone = make(chan struct{})

	tr.installHeartbeat(conn)

	// Start readLoop.
	go func() {
		defer close(tr.readDone)
		defer tr.closeDone()
		tr.readLoop()
	}()

	// pingPump should fail on first tick and close the conn, causing readLoop to exit.
	select {
	case <-tr.pingDone:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("pingDone not closed after write failure")
	}

	select {
	case <-tr.readDone:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("readLoop did not exit after ping write failure")
	}

	if err := tr.getTerminalError(); err == nil {
		t.Error("expected terminal error to be set after ping write failure")
	}

	if conn.closeCalled.Load() == 0 {
		t.Error("expected conn.Close to be called")
	}
}

// TestPingIntervalZeroNoPingPump verifies that pingInterval=0 does not start pingPump.
func TestPingIntervalZeroNoPingPump(t *testing.T) {
	conn := newMockHeartbeatConn()
	tr := newTestTransport(0, 30*time.Millisecond)

	tr.installHeartbeat(conn)

	if tr.pingDone != nil {
		t.Error("expected pingDone to be nil when pingInterval=0")
	}

	time.Sleep(20 * time.Millisecond)

	if conn.getPingCount() != 0 {
		t.Errorf("expected 0 pings, got %d", conn.getPingCount())
	}
}

// TestReadTimeoutZeroNoReadDeadline verifies that readTimeout=0 does not set read deadline.
func TestReadTimeoutZeroNoReadDeadline(t *testing.T) {
	conn := newMockHeartbeatConn()
	tr := newTestTransport(10*time.Millisecond, 0)

	tr.installHeartbeat(conn)

	if conn.getReadDeadlineCount() != 0 {
		t.Errorf("expected no read deadline set, got %d", conn.getReadDeadlineCount())
	}

	if conn.getPongHandler() != nil {
		t.Error("expected pongHandler to be nil when readTimeout=0")
	}
}

// TestCloseDoesNotHang verifies Close() returns promptly.
func TestCloseDoesNotHang(t *testing.T) {
	conn := newMockHeartbeatConn()
	tr := newTestTransport(10*time.Millisecond, 30*time.Millisecond)
	tr.wsConn = conn
	tr.connected = true
	tr.readDone = make(chan struct{})

	tr.installHeartbeat(conn)

	// Start readLoop.
	go func() {
		defer close(tr.readDone)
		defer tr.closeDone()
		tr.readLoop()
	}()

	done := make(chan struct{})
	go func() {
		tr.Close()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Close() hung")
	}
}

// TestCloseDoesNotHangPingIntervalZero verifies Close() returns promptly when
// pingDone is nil (pingInterval=0).
func TestCloseDoesNotHangPingIntervalZero(t *testing.T) {
	conn := newMockHeartbeatConn()
	tr := newTestTransport(0, 30*time.Millisecond)
	tr.wsConn = conn
	tr.connected = true
	tr.readDone = make(chan struct{})

	tr.installHeartbeat(conn)

	// Start readLoop.
	go func() {
		defer close(tr.readDone)
		defer tr.closeDone()
		tr.readLoop()
	}()

	done := make(chan struct{})
	go func() {
		tr.Close()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Close() hung with pingInterval=0")
	}
}

// TestCloseMustCloseWSToUnblockReadMessage verifies that Close() closes the
// underlying websocket so ReadMessage unblocks.
func TestCloseMustCloseWSToUnblockReadMessage(t *testing.T) {
	conn := newMockHeartbeatConn()
	tr := newTestTransport(0, 30*time.Millisecond)
	tr.wsConn = conn
	tr.connected = true
	tr.readDone = make(chan struct{})

	tr.installHeartbeat(conn)

	// Start readLoop.
	go func() {
		defer close(tr.readDone)
		defer tr.closeDone()
		tr.readLoop()
	}()

	// Close should cause conn.Close() which unblocks ReadMessage.
	tr.Close()

	select {
	case <-tr.readDone:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("readLoop not unblocked after Close()")
	}

	if conn.closeCalled.Load() == 0 {
		t.Error("expected conn.Close to be called by Close()")
	}
}

// TestCloseDoesNotSetTerminalError verifies that a local Close() does not
// leave a terminal error (the read error caused by closing should be suppressed).
func TestCloseDoesNotSetTerminalError(t *testing.T) {
	conn := newMockHeartbeatConn()
	tr := newTestTransport(10*time.Millisecond, 30*time.Millisecond)
	tr.wsConn = conn
	tr.connected = true
	tr.readDone = make(chan struct{})

	tr.installHeartbeat(conn)

	// Start readLoop.
	go func() {
		defer close(tr.readDone)
		defer tr.closeDone()
		tr.readLoop()
	}()

	// Close should exit cleanly.
	tr.Close()

	select {
	case <-tr.readDone:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("readLoop not unblocked after Close()")
	}

	if tr.pingDone != nil {
		<-tr.pingDone
	}

	if err := tr.getTerminalError(); err != nil {
		t.Errorf("expected no terminal error after local Close(), got: %v", err)
	}
}

// TestDataFrameRefreshesReadDeadline verifies readLoop calls SetReadDeadline
// after receiving a text frame.
func TestDataFrameRefreshesReadDeadline(t *testing.T) {
	conn := newMockHeartbeatConn()
	tr := newTestTransport(0, 30*time.Millisecond)
	tr.wsConn = conn
	tr.connected = true
	tr.readDone = make(chan struct{})

	tr.installHeartbeat(conn)

	// installHeartbeat sets initial read deadline, record count.
	initialCount := conn.getReadDeadlineCount()

	// Start readLoop.
	go func() {
		defer close(tr.readDone)
		defer tr.closeDone()
		tr.readLoop()
	}()

	// Send a text frame.
	conn.readCh <- readResult{msgType: websocket.TextMessage, data: []byte(`{"jsonrpc":"2.0"}`)}

	// Give readLoop a moment to process.
	time.Sleep(10 * time.Millisecond)

	afterCount := conn.getReadDeadlineCount()
	if afterCount <= initialCount {
		t.Errorf("expected SetReadDeadline count to increase after data frame; before=%d after=%d", initialCount, afterCount)
	}

	// Cleanup.
	tr.closeDone()
	conn.Close()
	<-tr.readDone
}

// TestWriteControlPingUsesControlWriteDeadline verifies the deadline passed to
// WriteControl for Ping is approximately wsutil.ControlWriteDeadline (5s) from now.
func TestWriteControlPingUsesControlWriteDeadline(t *testing.T) {
	conn := newMockHeartbeatConn()
	tr := newTestTransport(10*time.Millisecond, 50*time.Millisecond)
	tr.wsConn = conn
	tr.connected = true
	tr.readDone = make(chan struct{})

	tr.installHeartbeat(conn)

	// Wait for at least one ping.
	time.Sleep(15 * time.Millisecond)

	tr.closeDone()
	conn.Close()
	if tr.pingDone != nil {
		<-tr.pingDone
	}

	deadlines := conn.getPingDeadlines()
	if len(deadlines) == 0 {
		t.Fatal("no ping deadlines recorded")
	}

	// The deadline should be ~5s from when the ping was sent.
	// Since pings are recorded with time.Now() and deadline is time.Now().Add(5s),
	// the difference between deadline and pingSent should be ~5s.
	conn.mu.Lock()
	pingSent := conn.pingSent[0]
	deadline := conn.pingDeadlines[0]
	conn.mu.Unlock()

	diff := deadline.Sub(pingSent)
	// Allow generous tolerance for test scheduling.
	if diff < 4900*time.Millisecond || diff > 5100*time.Millisecond {
		t.Errorf("expected ping deadline ~5s from send time, got %v", diff)
	}
}

// TestNegativeConfigValuesIgnored verifies that negative ping/pong config
// values don't crash; they are silently ignored.
func TestNegativeConfigValuesIgnored(t *testing.T) {
	tr := newTestTransport(DefaultPingInterval, DefaultReadTimeout)

	// Apply negative options — should not panic.
	WithPingInterval(-1 * time.Second)(tr)
	WithReadTimeout(-5 * time.Second)(tr)

	// Values should remain at defaults.
	if tr.pingInterval != DefaultPingInterval {
		t.Errorf("expected pingInterval to remain default, got %v", tr.pingInterval)
	}
	if tr.readTimeout != DefaultReadTimeout {
		t.Errorf("expected readTimeout to remain default, got %v", tr.readTimeout)
	}
}

// TestConfigPingZeroPongPositiveDoesNotCrash verifies the warning path where
// PingInterval=0 with PongTimeout>0 doesn't crash.
func TestConfigPingZeroPongPositiveDoesNotCrash(t *testing.T) {
	// This just exercises the code path; the warning is logged but we verify no panic.
	tr := newTestTransport(0, 30*time.Millisecond)
	conn := newMockHeartbeatConn()

	tr.installHeartbeat(conn)

	// Should have set read deadline but no pingPump.
	if tr.pingDone != nil {
		t.Error("expected pingDone to be nil")
	}
	if conn.getReadDeadlineCount() == 0 {
		t.Error("expected read deadline to be set when readTimeout > 0")
	}

	_ = fmt.Sprintf("transport: %+v", tr) // use fmt import
}
