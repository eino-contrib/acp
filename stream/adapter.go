package stream

import (
	"bytes"
	"context"
	"io"
	"sync"
)

// NewPipe adapts a Streamer into an (io.ReadCloser, io.WriteCloser) pair that
// can feed the ACP stdio transport (stdio.NewTransport) without any changes
// to the Agent implementation.
//
// Framing: the stdio transport uses newline-delimited JSON (ndjson). NewPipe
// translates between that byte stream and Streamer's payload semantics:
//
//   - Reader: each ReadPayload result is emitted with a trailing '\n'. The
//     bufio.Scanner inside the stdio transport picks them up as lines.
//   - Writer: incoming bytes are scanned for '\n'. Every complete line is
//     passed to Streamer.WritePayload (trailing '\n' stripped). Partial
//     lines are buffered until the next Write provides the terminator.
//
// ctx governs the lifetime of the adapter. Closing either the reader or the
// writer cancels ctx and calls Streamer.Close exactly once — both returned
// handles share the same underlying Streamer.
//
// If a nil ctx is passed, context.Background is used.
func NewPipe(ctx context.Context, s Streamer) (io.ReadCloser, io.WriteCloser) {
	if ctx == nil {
		ctx = context.Background()
	}
	inner, cancel := context.WithCancel(ctx)
	p := &pipe{
		s:      s,
		ctx:    inner,
		cancel: cancel,
	}
	return &pipeReader{p: p}, &pipeWriter{p: p}
}

type pipe struct {
	s      Streamer
	ctx    context.Context
	cancel context.CancelFunc

	// reader state
	rmu     sync.Mutex
	rbuf    []byte // current payload + '\n' being drained
	rpos    int
	readErr error // sticky: once set, all future Reads return it after rbuf drains

	// writer state
	wmu  sync.Mutex
	wbuf bytes.Buffer // accumulator for partial lines across multiple Writes

	closeOnce sync.Once
	closeErr  error
}

type pipeReader struct{ p *pipe }

func (r *pipeReader) Read(buf []byte) (int, error) { return r.p.read(buf) }
func (r *pipeReader) Close() error                 { return r.p.Close() }

type pipeWriter struct{ p *pipe }

func (w *pipeWriter) Write(buf []byte) (int, error) { return w.p.write(buf) }
func (w *pipeWriter) Close() error                  { return w.p.Close() }

func (p *pipe) read(buf []byte) (int, error) {
	if len(buf) == 0 {
		return 0, nil
	}
	p.rmu.Lock()
	defer p.rmu.Unlock()

	if p.rpos >= len(p.rbuf) {
		// Fully drained the previous payload; either fetch the next one or
		// surface the sticky error.
		if p.readErr != nil {
			return 0, p.readErr
		}
		payload, err := p.s.ReadPayload(p.ctx)
		if err != nil {
			p.readErr = err
			if len(payload) == 0 {
				return 0, err
			}
			// Rare: peer returned bytes alongside EOF. Deliver them first,
			// surface the error on the next call.
		}
		// Defensive copy so we never mutate the slice returned by the
		// Streamer implementation when appending '\n'.
		p.rbuf = make([]byte, 0, len(payload)+1)
		p.rbuf = append(p.rbuf, payload...)
		p.rbuf = append(p.rbuf, '\n')
		p.rpos = 0
	}

	n := copy(buf, p.rbuf[p.rpos:])
	p.rpos += n
	return n, nil
}

func (p *pipe) write(buf []byte) (int, error) {
	if len(buf) == 0 {
		return 0, nil
	}
	p.wmu.Lock()
	defer p.wmu.Unlock()

	written := 0
	for len(buf) > 0 {
		idx := bytes.IndexByte(buf, '\n')
		if idx < 0 {
			// Partial line: append to the accumulator and wait for the
			// remainder on a later Write.
			p.wbuf.Write(buf)
			written += len(buf)
			return written, nil
		}

		// Complete line: accumulator + buf[:idx] -> one payload.
		p.wbuf.Write(buf[:idx])
		payload := make([]byte, p.wbuf.Len())
		copy(payload, p.wbuf.Bytes())
		p.wbuf.Reset()

		if err := p.s.WritePayload(p.ctx, payload); err != nil {
			// Report only the bytes actually pushed through; callers use the
			// short count to decide whether to retry.
			return written, err
		}
		written += idx + 1
		buf = buf[idx+1:]
	}
	return written, nil
}

func (p *pipe) Close() error {
	p.closeOnce.Do(func() {
		p.cancel()
		p.closeErr = p.s.Close("pipe closed")
	})
	return p.closeErr
}
