// Package stdio implements the ACP stdio transport (newline-delimited JSON).
package stdio

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	acplog "github.com/eino-contrib/acp/internal/log"
	"github.com/eino-contrib/acp/internal/safe"
	acptransport "github.com/eino-contrib/acp/transport"
)

const (
	defaultMaxMessageSize = 10 * 1024 * 1024 // 10MB
	initialBufSize        = 64 * 1024        // 64KB

	// defaultWriteTimeout caps the time WriteMessage will wait for the
	// writer goroutine to accept and flush a message when the caller
	// provided no deadline. Without this cap a slow or blocked io.Writer
	// (e.g. stdout pipe full, downstream reader paused) would back-pressure
	// into handler goroutines — because jsonrpc.Connection.respond writes
	// using the connection-level context, which has no deadline — and
	// eventually exhaust the worker pool. Mirrors the safety net in the
	// ws-server transport.
	defaultWriteTimeout = 10 * time.Second
)

// Option configures a stdio Transport.
type Option func(*Transport)

// WithMaxMessageSize sets the maximum size (in bytes) of a single inbound message.
// When maxSize <= 0, the default is used.
func WithMaxMessageSize(maxSize int) Option {
	return func(t *Transport) {
		if maxSize > 0 {
			t.maxMessageSize = maxSize
		}
	}
}

// WithInitialBufSize sets the initial scanner buffer size.
// When size <= 0, the default is used.
func WithInitialBufSize(size int) Option {
	return func(t *Transport) {
		if size > 0 {
			t.initialBufSize = size
		}
	}
}

// Transport implements the Transport interface over stdin/stdout using newline-delimited JSON.
type Transport struct {
	// reader is the source for reading inbound messages (typically stdin).
	reader io.Reader
	// writer is the destination for writing outbound messages (typically stdout).
	writer io.Writer

	// writeCh hands pending writes to the single writer goroutine so
	// WriteMessage can honour the caller's context while waiting for its
	// turn at the underlying writer. A dedicated goroutine is the only way
	// to interrupt the wait without leaking a mutex holder when the caller
	// times out (sync.Mutex is not selectable).
	writeCh chan writeRequest

	// readCh delivers read results from the background reading goroutine to consumers.
	readCh chan readResult
	// done signals the background reading goroutine to stop.
	done chan struct{}
	// closeOnce ensures the transport is closed exactly once.
	closeOnce sync.Once
	// startOnce ensures the background reading goroutine is started exactly once.
	startOnce sync.Once
	// writerStartOnce ensures the writer loop is started exactly once.
	writerStartOnce sync.Once

	// maxMessageSize is the maximum allowed size in bytes for a single inbound message.
	maxMessageSize int
	// initialBufSize is the initial buffer size for the scanner.
	initialBufSize int
}

type readResult struct {
	msg json.RawMessage
	err error
}

// writeRequest is a single outbound message handed to writerLoop via
// writeCh. resp is buffered so writerLoop never blocks delivering the
// result even if the caller has already returned (ctx cancellation).
type writeRequest struct {
	buf  []byte
	ctx  context.Context
	resp chan error
}

var _ acptransport.Transport = (*Transport)(nil)

// NewTransport creates a stdio transport from reader/writer pair.
func NewTransport(reader io.Reader, writer io.Writer, opts ...Option) *Transport {
	return newTransport(reader, writer, opts...)
}

func newTransport(reader io.Reader, writer io.Writer, opts ...Option) *Transport {
	t := &Transport{
		reader:         reader,
		writer:         writer,
		readCh:         make(chan readResult, 1),
		writeCh:        make(chan writeRequest),
		done:           make(chan struct{}),
		maxMessageSize: defaultMaxMessageSize,
		initialBufSize: initialBufSize,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(t)
		}
	}
	return t
}

// ensureStarted starts the readLoop goroutine exactly once, on the first call
// to ReadMessage. This avoids consuming input before the caller has wired up
// the connection layer.
func (t *Transport) ensureStarted() {
	t.startOnce.Do(func() {
		safe.Go(t.readLoop)
	})
}

// ReadMessage reads the next newline-delimited JSON message.
func (t *Transport) ReadMessage(ctx context.Context) (json.RawMessage, error) {
	t.ensureStarted()

	select {
	case res, ok := <-t.readCh:
		if !ok {
			return nil, io.EOF
		}
		if res.err == nil && len(res.msg) > 0 {
			acplog.Access(ctx, "stdio", acplog.AccessDirectionRecv, res.msg)
		}
		return res.msg, res.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-t.done:
		return nil, io.EOF
	}
}

// ensureWriterStarted starts the writerLoop goroutine exactly once, on the
// first call to WriteMessage. This keeps the transport symmetric with
// ensureStarted (read side) and avoids a writer goroutine for read-only
// deployments.
func (t *Transport) ensureWriterStarted() {
	t.writerStartOnce.Do(func() {
		safe.Go(t.writerLoop)
	})
}

// WriteMessage writes a JSON message followed by a newline. The caller's
// context governs both the wait to hand the message to the writer goroutine
// and the wait for the write to finish; a cancelled ctx returns promptly
// even when the underlying writer is blocked (e.g. stdout pipe full,
// downstream reader paused). The write itself may still complete later on
// the writer goroutine because an io.Writer has no interrupt primitive.
//
// If the caller provided no ctx deadline the wait is capped at
// defaultWriteTimeout so a stalled peer cannot indefinitely block the caller
// (most notably jsonrpc.Connection.respond, which uses the connection-level
// context that has no deadline).
func (t *Transport) WriteMessage(ctx context.Context, data json.RawMessage) error {
	select {
	case <-t.done:
		return acptransport.ErrTransportClosed
	default:
	}

	acplog.Access(ctx, "stdio", acplog.AccessDirectionSend, data)

	buf := make([]byte, 0, len(data)+1)
	buf = append(buf, data...)
	buf = append(buf, '\n')

	t.ensureWriterStarted()

	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultWriteTimeout)
		defer cancel()
	}

	resp := make(chan error, 1)
	req := writeRequest{buf: buf, ctx: ctx, resp: resp}

	select {
	case <-t.done:
		return acptransport.ErrTransportClosed
	case <-ctx.Done():
		return ctx.Err()
	case t.writeCh <- req:
	}

	select {
	case err := <-resp:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-t.done:
		return acptransport.ErrTransportClosed
	}
}

// Close closes the transport.
func (t *Transport) Close() error {
	t.closeOnce.Do(func() {
		close(t.done)

		if c, ok := t.reader.(io.Closer); ok {
			if err := c.Close(); err != nil {
				acplog.Error("close stdio reader: %v", err)
			}
		}
		if c, ok := t.writer.(io.Closer); ok {
			if err := c.Close(); err != nil {
				acplog.Error("close stdio writer: %v", err)
			}
		}
	})
	return nil
}

func (t *Transport) readLoop() {
	scanner := bufio.NewScanner(t.reader)
	maxSize := t.maxMessageSize
	if maxSize <= 0 {
		maxSize = defaultMaxMessageSize
	}
	bufSize := t.initialBufSize
	if bufSize <= 0 {
		bufSize = initialBufSize
	}
	// bufio.Scanner treats max < len(buf) as max=len(buf). Clamp the initial
	// buffer to maxSize so maxSize is always enforced.
	if maxSize > 0 && bufSize > maxSize {
		bufSize = maxSize
	}
	scanner.Buffer(make([]byte, bufSize), maxSize)
	defer close(t.readCh)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		msg := make(json.RawMessage, len(line))
		copy(msg, line)
		if !t.pushReadResult(readResult{msg: msg}) {
			return
		}
	}

	if err := scanner.Err(); err != nil {
		if ok := t.pushReadResult(readResult{err: fmt.Errorf("read: %w", err)}); !ok {
			acplog.Debug("drop stdio read error because transport is closed: %v", err)
		}
		return
	}
	if ok := t.pushReadResult(readResult{err: io.EOF}); !ok {
		acplog.Debug("drop stdio EOF because transport is closed")
	}
}

func (t *Transport) pushReadResult(res readResult) bool {
	select {
	case <-t.done:
		return false
	case t.readCh <- res:
		return true
	}
}

// writerLoop serialises outbound writes on a single goroutine so callers can
// select on their context while waiting for a turn. Writes run in arrival
// order. A closed done channel terminates the loop; in-flight callers fall
// through the done-observing selects in WriteMessage and see
// ErrTransportClosed.
func (t *Transport) writerLoop() {
	for {
		select {
		case <-t.done:
			return
		case req := <-t.writeCh:
			if req.ctx != nil {
				if err := req.ctx.Err(); err != nil {
					req.resp <- err
					continue
				}
			}
			_, err := t.writer.Write(req.buf)
			req.resp <- err
		}
	}
}
