package httpserver

import (
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"

	acp "github.com/eino-contrib/acp"
	"github.com/eino-contrib/acp/internal/jsonrpc"
	acplog "github.com/eino-contrib/acp/internal/log"
)

// SSEWriter — writes JSON-RPC messages as SSE events on a POST response.

// errWriterClosed indicates the SSE writer has been closed.
var errWriterClosed = errors.New("SSE writer closed")

// SSEWriter writes JSON-RPC messages as SSE events to an HTTP POST response.
// It is used locally in the direct-dispatch POST handler to write the final
// JSON-RPC response back to the client.
type SSEWriter struct {
	ctx    HandlerContext
	mu     sync.Mutex
	closed atomic.Bool
}

// NewSSEWriter creates a new SSEWriter bound to a handler context.
func NewSSEWriter(ctx HandlerContext) *SSEWriter {
	return &SSEWriter{ctx: ctx}
}

// WriteResponse writes a JSON-RPC response (success or error) as an SSE event.
// id is the raw JSON-RPC request ID from the original request.
func (w *SSEWriter) WriteResponse(id *json.RawMessage, result any, rpcErr *acp.RPCError) error {
	parsedID := rawToJSONRPCID(id)
	var msg *jsonrpc.Message
	if rpcErr != nil {
		msg = jsonrpc.NewErrorResponse(parsedID, rpcErr)
	} else {
		var err error
		msg, err = jsonrpc.NewResponse(parsedID, result)
		if err != nil {
			return err
		}
	}
	return w.writeMessage(msg)
}

// Close marks the writer as closed.
func (w *SSEWriter) Close() {
	w.closed.Store(true)
}

func (w *SSEWriter) writeMessage(msg *jsonrpc.Message) error {
	if w.closed.Load() {
		return errWriterClosed
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	acplog.Access(w.ctx.Context(), "http-server", acplog.AccessDirectionSend, data)
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.ctx.WriteSSEEvent(data); err != nil {
		return err
	}
	w.ctx.Flush()
	return nil
}

// rawToJSONRPCID converts a raw JSON id value to a jsonrpc.ID pointer.
func rawToJSONRPCID(raw *json.RawMessage) *jsonrpc.ID {
	if raw == nil {
		return nil
	}
	var id jsonrpc.ID
	if err := json.Unmarshal(*raw, &id); err != nil {
		return nil
	}
	return &id
}
