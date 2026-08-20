package proxy

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eino-contrib/acp/internal/wsconn"
	"github.com/eino-contrib/acp/stream"
)

type lifecycleFactoryFunc func(context.Context, map[string]string) (stream.Streamer, error)

func (f lifecycleFactoryFunc) NewStreamer(ctx context.Context, meta map[string]string) (stream.Streamer, error) {
	return f(ctx, meta)
}

type typedNilFactory struct{}

func (*typedNilFactory) NewStreamer(context.Context, map[string]string) (stream.Streamer, error) {
	return newLifecycleStreamer(), nil
}

func TestNewACPProxyRejectsTypedNilFactory(t *testing.T) {
	var factory *typedNilFactory
	if _, err := NewACPProxy(factory); err == nil {
		t.Fatal("NewACPProxy accepted a typed-nil factory")
	}
}

type lifecycleStreamer struct {
	closed    chan struct{}
	closeOnce sync.Once
}

func newLifecycleStreamer() *lifecycleStreamer {
	return &lifecycleStreamer{closed: make(chan struct{})}
}

func (*lifecycleStreamer) WritePayload(context.Context, []byte) error { return nil }

func (s *lifecycleStreamer) ReadPayload(ctx context.Context) ([]byte, error) {
	select {
	case <-s.closed:
		return nil, io.EOF
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *lifecycleStreamer) Close(string) error {
	s.closeOnce.Do(func() { close(s.closed) })
	return nil
}

type lifecycleConn struct {
	closed    chan struct{}
	closeOnce sync.Once
	closeCode atomic.Int64
	closeText atomic.Value
}

func newLifecycleConn() *lifecycleConn {
	return &lifecycleConn{closed: make(chan struct{})}
}

func (c *lifecycleConn) ReadMessage() (int, []byte, error) {
	<-c.closed
	return 0, nil, io.EOF
}

func (*lifecycleConn) WriteMessage(int, []byte) error { return nil }

func (c *lifecycleConn) WriteControl(messageType int, payload []byte, _ time.Time) error {
	if messageType == wsconn.CloseMessage && len(payload) >= 2 {
		c.closeCode.Store(int64(binary.BigEndian.Uint16(payload[:2])))
		c.closeText.Store(string(payload[2:]))
	}
	return nil
}

func (*lifecycleConn) SetReadLimit(int64)                {}
func (*lifecycleConn) SetReadDeadline(time.Time) error   { return nil }
func (*lifecycleConn) SetWriteDeadline(time.Time) error  { return nil }
func (*lifecycleConn) SetPingHandler(func(string) error) {}
func (c *lifecycleConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}

func TestAdmissionMetadataSnapshotAndContext(t *testing.T) {
	type contextKey struct{}
	returned := map[string]string{"mutable": "before"}
	gotMeta := make(chan map[string]string, 1)
	gotValue := make(chan any, 1)
	factory := lifecycleFactoryFunc(func(ctx context.Context, meta map[string]string) (stream.Streamer, error) {
		gotValue <- ctx.Value(contextKey{})
		gotMeta <- meta
		return nil, errors.New("stop")
	})
	p, err := NewACPProxy(factory, WithMetadataExtractor(func(ctx context.Context, headers HeaderGetter) map[string]string {
		if got := ctx.Value(contextKey{}); got != "trace-value" {
			t.Fatalf("extractor context value = %v", got)
		}
		returned["Authorization"] = headers.Get("Authorization")
		return returned
	}))
	if err != nil {
		t.Fatalf("NewACPProxy: %v", err)
	}
	requestCtx, requestCancel := context.WithCancel(context.WithValue(context.Background(), contextKey{}, "trace-value"))
	admission, err := p.Admit(requestCtx, func(name string) string {
		if name == "Authorization" {
			return "Bearer test"
		}
		return ""
	})
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	returned["mutable"] = "after"
	requestCancel()
	admission.Serve(newLifecycleConn())

	if got := <-gotValue; got != "trace-value" {
		t.Fatalf("factory context value = %v", got)
	}
	meta := <-gotMeta
	if meta["Authorization"] != "Bearer test" || meta["mutable"] != "before" {
		t.Fatalf("factory metadata = %#v", meta)
	}
}

func TestAdmissionMetadataExtractorPanicReleasesSlot(t *testing.T) {
	p, err := NewACPProxy(lifecycleFactoryFunc(func(context.Context, map[string]string) (stream.Streamer, error) {
		t.Fatal("factory must not run")
		return nil, nil
	}),
		WithMaxConcurrentConnections(1),
		WithMetadataExtractor(func(context.Context, HeaderGetter) map[string]string { panic("secret panic") }),
	)
	if err != nil {
		t.Fatalf("NewACPProxy: %v", err)
	}
	if _, err := p.Admit(context.Background(), nil); err == nil {
		t.Fatal("Admit error = nil, want metadata panic error")
	}
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

func TestMetadataExtractorObservesProxyClose(t *testing.T) {
	extractorStarted := make(chan struct{})
	extractorCanceled := make(chan struct{})
	p, err := NewACPProxy(lifecycleFactoryFunc(func(context.Context, map[string]string) (stream.Streamer, error) {
		return nil, errors.New("factory must not run")
	}), WithMetadataExtractor(func(ctx context.Context, _ HeaderGetter) map[string]string {
		close(extractorStarted)
		<-ctx.Done()
		close(extractorCanceled)
		return nil
	}))
	if err != nil {
		t.Fatalf("NewACPProxy: %v", err)
	}
	admitResult := make(chan error, 1)
	go func() {
		_, admitErr := p.Admit(context.Background(), nil)
		admitResult <- admitErr
	}()
	<-extractorStarted
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-extractorCanceled:
	case <-time.After(time.Second):
		t.Fatal("metadata extractor did not observe proxy Close")
	}
	if err := <-admitResult; !errors.Is(err, ErrClosed) {
		t.Fatalf("Admit error = %v, want ErrClosed", err)
	}
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

func TestAdmissionLimitAndAbortRelease(t *testing.T) {
	p, err := NewACPProxy(lifecycleFactoryFunc(func(context.Context, map[string]string) (stream.Streamer, error) {
		return nil, errors.New("factory must not run")
	}), WithMaxConcurrentConnections(1))
	if err != nil {
		t.Fatalf("NewACPProxy: %v", err)
	}
	first, err := p.Admit(context.Background(), nil)
	if err != nil {
		t.Fatalf("first Admit: %v", err)
	}
	if _, err := p.Admit(context.Background(), nil); !errors.Is(err, ErrTooManyConnections) {
		t.Fatalf("second Admit error = %v, want ErrTooManyConnections", err)
	}
	first.Abort()
	first.Abort()
	third, err := p.Admit(context.Background(), nil)
	if err != nil {
		t.Fatalf("Admit after Abort: %v", err)
	}
	third.Abort()
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

func TestFactoryFailureModesClose1011(t *testing.T) {
	tests := []struct {
		name       string
		factory    stream.StreamerFactory
		wantReason string
	}{
		{name: "error", factory: lifecycleFactoryFunc(func(context.Context, map[string]string) (stream.Streamer, error) {
			return nil, errors.New("dial refused")
		}), wantReason: "upstream: dial refused"},
		{name: "nil", factory: lifecycleFactoryFunc(func(context.Context, map[string]string) (stream.Streamer, error) {
			return nil, nil
		}), wantReason: "upstream: streamer factory returned nil streamer"},
		{name: "typed nil", factory: lifecycleFactoryFunc(func(context.Context, map[string]string) (stream.Streamer, error) {
			var streamer *lifecycleStreamer
			return streamer, nil
		}), wantReason: "upstream: streamer factory returned nil streamer"},
		{name: "panic", factory: lifecycleFactoryFunc(func(context.Context, map[string]string) (stream.Streamer, error) {
			panic("must not leak panic detail")
		}), wantReason: "upstream: internal error"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			p, err := NewACPProxy(test.factory)
			if err != nil {
				t.Fatalf("NewACPProxy: %v", err)
			}
			admission, err := p.Admit(context.Background(), nil)
			if err != nil {
				t.Fatalf("Admit: %v", err)
			}
			conn := newLifecycleConn()
			admission.Serve(conn)
			if got := int(conn.closeCode.Load()); got != wsconn.CloseInternalServerErr {
				t.Fatalf("close code = %d, want %d", got, wsconn.CloseInternalServerErr)
			}
			gotReason, _ := conn.closeText.Load().(string)
			if gotReason != test.wantReason {
				t.Fatalf("close reason = %q, want %q", gotReason, test.wantReason)
			}
		})
	}
}

func TestFactoryErrorWithStreamerClosesWebSocketBeforeBlockingStreamerClose(t *testing.T) {
	streamer := &blockingCloseStreamer{
		closeStarted: make(chan struct{}),
		releaseClose: make(chan struct{}),
	}
	p, err := NewACPProxy(lifecycleFactoryFunc(func(context.Context, map[string]string) (stream.Streamer, error) {
		return streamer, errors.New("dial partially failed")
	}))
	if err != nil {
		t.Fatalf("NewACPProxy: %v", err)
	}
	admission, err := p.Admit(context.Background(), nil)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	conn := newLifecycleConn()
	serveDone := make(chan struct{})
	go func() {
		admission.Serve(conn)
		close(serveDone)
	}()
	select {
	case <-streamer.closeStarted:
	case <-time.After(time.Second):
		t.Fatal("Streamer.Close was not called after factory error")
	}
	select {
	case <-conn.closed:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("north-bound websocket remained open while failed Streamer.Close was blocked")
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := p.Shutdown(shutdownCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown with blocked failed Streamer.Close = %v, want deadline", err)
	}
	close(streamer.releaseClose)
	select {
	case <-serveDone:
	case <-time.After(time.Second):
		t.Fatal("Serve did not finish after failed Streamer.Close unblocked")
	}
}

func TestFactorySuccessRacingCloseClosesWebSocketBeforeBlockingStreamerClose(t *testing.T) {
	streamer := &blockingCloseStreamer{
		closeStarted: make(chan struct{}),
		releaseClose: make(chan struct{}),
	}
	factoryEntered := make(chan struct{})
	releaseFactory := make(chan struct{})
	p, err := NewACPProxy(lifecycleFactoryFunc(func(context.Context, map[string]string) (stream.Streamer, error) {
		close(factoryEntered)
		<-releaseFactory
		return streamer, nil
	}))
	if err != nil {
		t.Fatalf("NewACPProxy: %v", err)
	}
	admission, err := p.Admit(context.Background(), nil)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	conn := newLifecycleConn()
	serveDone := make(chan struct{})
	go func() {
		admission.Serve(conn)
		close(serveDone)
	}()
	select {
	case <-factoryEntered:
	case <-time.After(time.Second):
		t.Fatal("factory did not start")
	}

	// Serve has already published a.ws before entering the factory. Hold the
	// admission lock so the successful factory result cannot publish active
	// until Close has atomically marked the admission as closing.
	admission.mu.Lock()
	close(releaseFactory)
	if err := p.Close(); err != nil {
		admission.mu.Unlock()
		t.Fatalf("Close: %v", err)
	}
	admission.mu.Unlock()

	select {
	case <-streamer.closeStarted:
	case <-time.After(time.Second):
		t.Fatal("Streamer.Close was not called in factory-success/Close race")
	}
	select {
	case <-conn.closed:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("north-bound websocket remained open in factory-success/Close race")
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := p.Shutdown(shutdownCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown with blocked race Streamer.Close = %v, want deadline", err)
	}
	close(streamer.releaseClose)
	select {
	case <-serveDone:
	case <-time.After(time.Second):
		t.Fatal("Serve did not finish after race Streamer.Close unblocked")
	}
}

func TestFactoryTimeoutAndShutdownDeadline(t *testing.T) {
	factoryStarted := make(chan struct{})
	releaseFactory := make(chan struct{})
	p, err := NewACPProxy(lifecycleFactoryFunc(func(context.Context, map[string]string) (stream.Streamer, error) {
		close(factoryStarted)
		<-releaseFactory
		return nil, errors.New("late")
	}), WithHandshakeTimeout(20*time.Millisecond))
	if err != nil {
		t.Fatalf("NewACPProxy: %v", err)
	}
	admission, err := p.Admit(context.Background(), nil)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	conn := newLifecycleConn()
	serveDone := make(chan struct{})
	go func() {
		admission.Serve(conn)
		close(serveDone)
	}()
	<-factoryStarted
	eventually(t, time.Second, func() bool {
		return conn.closeCode.Load() == wsconn.CloseInternalServerErr
	})

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := p.Shutdown(shutdownCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown error = %v, want deadline exceeded", err)
	}
	close(releaseFactory)
	select {
	case <-serveDone:
	case <-time.After(time.Second):
		t.Fatal("Serve did not return after factory released")
	}
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("second Shutdown: %v", err)
	}
}

func TestCloseIdempotentCoversAdmittedAndActive(t *testing.T) {
	t.Run("admitted before upgrade", func(t *testing.T) {
		p, err := NewACPProxy(lifecycleFactoryFunc(func(context.Context, map[string]string) (stream.Streamer, error) {
			t.Fatal("factory must not run")
			return nil, nil
		}), WithMaxConcurrentConnections(1))
		if err != nil {
			t.Fatalf("NewACPProxy: %v", err)
		}
		admission, err := p.Admit(context.Background(), nil)
		if err != nil {
			t.Fatalf("Admit: %v", err)
		}
		for i := 0; i < 10; i++ {
			if err := p.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
		}
		if _, err := p.Admit(context.Background(), nil); !errors.Is(err, ErrClosed) {
			t.Fatalf("post-close Admit error = %v", err)
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		if err := p.Shutdown(shutdownCtx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Shutdown before upgrade outcome = %v, want deadline", err)
		}
		cancel()
		lateConn := newLifecycleConn()
		admission.Serve(lateConn)
		select {
		case <-lateConn.closed:
		case <-time.After(time.Second):
			t.Fatal("late Serve did not close orphan websocket")
		}
		if got := int(lateConn.closeCode.Load()); got != wsconn.CloseGoingAway {
			t.Fatalf("late Serve close code = %d, want %d", got, wsconn.CloseGoingAway)
		}
		if err := p.Shutdown(context.Background()); err != nil {
			t.Fatalf("Shutdown after late Serve: %v", err)
		}
	})

	t.Run("active", func(t *testing.T) {
		streamer := newLifecycleStreamer()
		factoryStarted := make(chan struct{})
		p, err := NewACPProxy(lifecycleFactoryFunc(func(context.Context, map[string]string) (stream.Streamer, error) {
			close(factoryStarted)
			return streamer, nil
		}))
		if err != nil {
			t.Fatalf("NewACPProxy: %v", err)
		}
		admission, err := p.Admit(context.Background(), nil)
		if err != nil {
			t.Fatalf("Admit: %v", err)
		}
		conn := newLifecycleConn()
		serveDone := make(chan struct{})
		go func() {
			admission.Serve(conn)
			close(serveDone)
		}()
		<-factoryStarted
		for i := 0; i < 10; i++ {
			if err := p.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := p.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
		<-serveDone
	})
}

func TestCloseDoesNotWaitForAdmissionLock(t *testing.T) {
	p, err := NewACPProxy(lifecycleFactoryFunc(func(context.Context, map[string]string) (stream.Streamer, error) {
		return nil, errors.New("factory must not run")
	}))
	if err != nil {
		t.Fatalf("NewACPProxy: %v", err)
	}
	admission, err := p.Admit(context.Background(), nil)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	admission.mu.Lock()
	closeDone := make(chan struct{})
	go func() {
		_ = p.Close()
		close(closeDone)
	}()
	select {
	case <-closeDone:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Close waited for admission lock")
	}
	admission.mu.Unlock()
	admission.Abort()
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

type blockingCloseStreamer struct {
	closeStarted chan struct{}
	releaseClose chan struct{}
	closeOnce    sync.Once
}

func (*blockingCloseStreamer) WritePayload(context.Context, []byte) error { return nil }
func (s *blockingCloseStreamer) ReadPayload(ctx context.Context) ([]byte, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (s *blockingCloseStreamer) Close(string) error {
	s.closeOnce.Do(func() {
		close(s.closeStarted)
		<-s.releaseClose
	})
	return nil
}

func TestCloseDoesNotWaitForBlockingStreamerClose(t *testing.T) {
	streamer := &blockingCloseStreamer{
		closeStarted: make(chan struct{}),
		releaseClose: make(chan struct{}),
	}
	factoryStarted := make(chan struct{})
	p, err := NewACPProxy(lifecycleFactoryFunc(func(context.Context, map[string]string) (stream.Streamer, error) {
		close(factoryStarted)
		return streamer, nil
	}))
	if err != nil {
		t.Fatalf("NewACPProxy: %v", err)
	}
	admission, err := p.Admit(context.Background(), nil)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	serveDone := make(chan struct{})
	conn := newLifecycleConn()
	go func() {
		admission.Serve(conn)
		close(serveDone)
	}()
	<-factoryStarted
	eventually(t, time.Second, func() bool {
		admission.mu.Lock()
		defer admission.mu.Unlock()
		return admission.active != nil
	})

	closeDone := make(chan struct{})
	go func() {
		_ = p.Close()
		close(closeDone)
	}()
	select {
	case <-closeDone:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Close waited for Streamer.Close")
	}
	select {
	case <-streamer.closeStarted:
	case <-time.After(time.Second):
		t.Fatal("Streamer.Close was not started asynchronously")
	}
	select {
	case <-conn.closed:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("north-bound websocket remained open while Streamer.Close was blocked")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := p.Shutdown(shutdownCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown with blocked Streamer.Close = %v, want deadline", err)
	}
	close(streamer.releaseClose)
	select {
	case <-serveDone:
	case <-time.After(time.Second):
		t.Fatal("Serve did not finish after Streamer.Close unblocked")
	}
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("final Shutdown: %v", err)
	}
}

type panicCloseStreamer struct{ closed chan struct{} }

func (*panicCloseStreamer) WritePayload(context.Context, []byte) error { return nil }
func (s *panicCloseStreamer) ReadPayload(context.Context) ([]byte, error) {
	<-s.closed
	return nil, io.EOF
}
func (s *panicCloseStreamer) Close(string) error {
	select {
	case <-s.closed:
	default:
		close(s.closed)
	}
	panic("close panic")
}

func TestStreamerClosePanicStillClosesWebSocketAndDrains(t *testing.T) {
	streamer := &panicCloseStreamer{closed: make(chan struct{})}
	p, err := NewACPProxy(lifecycleFactoryFunc(func(context.Context, map[string]string) (stream.Streamer, error) {
		return streamer, nil
	}))
	if err != nil {
		t.Fatalf("NewACPProxy: %v", err)
	}
	admission, err := p.Admit(context.Background(), nil)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	conn := newLifecycleConn()
	done := make(chan struct{})
	go func() {
		admission.Serve(conn)
		close(done)
	}()
	eventually(t, time.Second, func() bool {
		admission.mu.Lock()
		defer admission.mu.Unlock()
		return admission.active != nil
	})
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := p.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	select {
	case <-conn.closed:
	default:
		t.Fatal("websocket remained open after Streamer.Close panic")
	}
	<-done
}

func eventually(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition was not satisfied before timeout")
}
