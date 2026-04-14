package stdio

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"
)

func TestTransportReadWriteMessage(t *testing.T) {
	reader := bytes.NewBufferString("\n{\"jsonrpc\":\"2.0\",\"method\":\"ping\"}\n")
	writer := &bytes.Buffer{}
	transport := NewTransport(reader, writer)

	msg, err := transport.ReadMessage(context.Background())
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if got, want := string(msg), `{"jsonrpc":"2.0","method":"ping"}`; got != want {
		t.Fatalf("message = %s, want %s", got, want)
	}

	outbound := json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":{}}`)
	if err := transport.WriteMessage(context.Background(), outbound); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
	if got, want := writer.String(), string(outbound)+"\n"; got != want {
		t.Fatalf("writer = %q, want %q", got, want)
	}
}

func TestTransportClosePreventsWrites(t *testing.T) {
	transport := NewTransport(bytes.NewBuffer(nil), &bytes.Buffer{})
	if err := transport.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := transport.WriteMessage(context.Background(), json.RawMessage(`{}`)); err == nil {
		t.Fatal("expected write after close to fail")
	}
}

func TestTransportReadEOF(t *testing.T) {
	transport := NewTransport(bytes.NewBuffer(nil), &bytes.Buffer{})
	_, err := transport.ReadMessage(context.Background())
	if err == nil {
		t.Fatal("expected EOF")
	}
	if err != io.EOF {
		t.Fatalf("error = %v, want EOF", err)
	}
}

func TestTransportReadRespectsContextCancellation(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()

	transport := NewTransport(reader, &bytes.Buffer{})
	defer transport.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := transport.ReadMessage(ctx)
	if err == nil {
		t.Fatal("expected context cancellation error")
	}
	if err != context.DeadlineExceeded {
		t.Fatalf("error = %v, want %v", err, context.DeadlineExceeded)
	}
}

func TestTransportMaxMessageSize(t *testing.T) {
	line := bytes.Repeat([]byte{'a'}, 128)
	reader := bytes.NewBuffer(append(line, '\n'))

	transport := NewTransport(reader, &bytes.Buffer{}, WithMaxMessageSize(32))
	defer transport.Close()

	_, err := transport.ReadMessage(context.Background())
	if err == nil {
		t.Fatal("expected read error")
	}
	if !errors.Is(err, bufio.ErrTooLong) {
		t.Fatalf("error = %v, want errors.Is(bufio.ErrTooLong)", err)
	}
}
