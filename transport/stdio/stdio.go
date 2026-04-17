// Package stdio implements the ACP stdio transport (newline-delimited JSON).
package stdio

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"

	acplog "github.com/eino-contrib/acp/internal/log"
	"github.com/eino-contrib/acp/internal/safe"
	acptransport "github.com/eino-contrib/acp/transport"
)

const (
	defaultMaxMessageSize = 50 * 1024 * 1024 // 50MB
	initialBufSize        = 64 * 1024        // 64KB
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

// WithLogger sets the logger for the stdio transport.
func WithLogger(logger acplog.Logger) Option {
	return func(t *Transport) {
		if logger != nil {
			t.logger = logger
		}
	}
}

// Transport implements the Transport interface over stdin/stdout using newline-delimited JSON.
type Transport struct {
	// reader is the source for reading inbound messages (typically stdin).
	reader io.Reader
	// writer is the destination for writing outbound messages (typically stdout).
	writer io.Writer
	// logger is used for logging transport-level diagnostics.
	logger acplog.Logger

	// writeMu guards concurrent writes to writer.
	writeMu sync.Mutex

	// readCh delivers read results from the background reading goroutine to consumers.
	readCh chan readResult
	// done signals the background reading goroutine to stop.
	done chan struct{}
	// closeOnce ensures the transport is closed exactly once.
	closeOnce sync.Once
	// startOnce ensures the background reading goroutine is started exactly once.
	startOnce sync.Once

	// maxMessageSize is the maximum allowed size in bytes for a single inbound message.
	maxMessageSize int
	// initialBufSize is the initial buffer size for the scanner.
	initialBufSize int
}

type readResult struct {
	msg json.RawMessage
	err error
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
		logger:         acplog.Default(),
		readCh:         make(chan readResult, 1),
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
		safe.GoWithLogger(acplog.OrDefault(t.logger), t.readLoop)
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
		return res.msg, res.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-t.done:
		return nil, io.EOF
	}
}

// WriteMessage writes a JSON message followed by a newline.
func (t *Transport) WriteMessage(_ context.Context, data json.RawMessage) error {
	select {
	case <-t.done:
		return acptransport.ErrTransportClosed
	default:
	}

	t.writeMu.Lock()
	defer t.writeMu.Unlock()

	// Double-check after acquiring the write lock, in case Close() ran
	// between the select above and the lock acquisition.
	select {
	case <-t.done:
		return acptransport.ErrTransportClosed
	default:
	}

	buf := make([]byte, 0, len(data)+1)
	buf = append(buf, data...)
	buf = append(buf, '\n')
	_, err := t.writer.Write(buf)
	return err
}

// Close closes the transport.
func (t *Transport) Close() error {
	t.closeOnce.Do(func() {
		close(t.done)

		logger := acplog.OrDefault(t.logger)
		if c, ok := t.reader.(io.Closer); ok {
			if err := c.Close(); err != nil {
				logger.Error("close stdio reader: %v", err)
			}
		}
		if c, ok := t.writer.(io.Closer); ok {
			if err := c.Close(); err != nil {
				logger.Error("close stdio writer: %v", err)
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

	logger := acplog.OrDefault(t.logger)

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
			logger.Info("drop stdio read error because transport is closed: %v", err)
		}
		return
	}
	if ok := t.pushReadResult(readResult{err: io.EOF}); !ok {
		logger.Info("drop stdio EOF because transport is closed")
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
