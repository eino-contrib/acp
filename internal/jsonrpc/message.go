package jsonrpc

import (
	"bytes"
	"encoding/json"
	"fmt"

	acp "github.com/eino-contrib/acp"
)

const Version = "2.0"

// Message represents a JSON-RPC 2.0 message (request, response, or notification).
//
// Serialization note: for requests and responses the "id" field is always
// emitted (including "id":null for parse-error responses where the id is
// unknown).  For notifications the "id" field is omitted entirely, as required
// by the JSON-RPC 2.0 specification.
type Message struct {
	JSONRPC string          `json:"-"`
	ID      *ID             `json:"-"`
	Method  string          `json:"-"`
	Params  json.RawMessage `json:"-"`
	Result  json.RawMessage `json:"-"`
	Error   *acp.RPCError   `json:"-"`

	// hasID distinguishes "no id" (notification) from "id is null" (parse
	// error response).  Set to true by NewRequest, NewResponse, and
	// NewErrorResponse.
	hasID bool
}

// MarshalJSON implements custom JSON marshaling so that:
//   - Requests/responses always include "id" (even when nil → null).
//   - Notifications never include "id".
func (m *Message) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')

	// jsonrpc (always present)
	buf.WriteString(`"jsonrpc":"`)
	buf.WriteString(m.JSONRPC)
	buf.WriteByte('"')

	// id: present for requests and responses, absent for notifications.
	if m.hasID {
		buf.WriteString(`,"id":`)
		if m.ID == nil {
			buf.WriteString("null")
		} else {
			idBytes, err := m.ID.MarshalJSON()
			if err != nil {
				return nil, err
			}
			buf.Write(idBytes)
		}
	}

	if m.Method != "" {
		buf.WriteString(`,"method":`)
		methodBytes, _ := json.Marshal(m.Method) // json.Marshal(string) cannot fail
		buf.Write(methodBytes)
	}
	if len(m.Params) > 0 {
		buf.WriteString(`,"params":`)
		buf.Write(m.Params)
	}
	if len(m.Result) > 0 {
		buf.WriteString(`,"result":`)
		buf.Write(m.Result)
	}
	if m.Error != nil {
		buf.WriteString(`,"error":`)
		errBytes, err := json.Marshal(m.Error)
		if err != nil {
			return nil, err
		}
		buf.Write(errBytes)
	}

	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// UnmarshalJSON implements custom JSON unmarshaling that mirrors MarshalJSON.
//
// This uses a struct-based intermediate representation instead of
// map[string]json.RawMessage to reduce allocations on the hot path.
func (m *Message) UnmarshalJSON(data []byte) error {
	// rawMessage is a struct-based intermediate for zero-map unmarshaling.
	// The idPresent sentinel detects whether "id" was present in the JSON
	// (distinguishing notification from request/response with id:null).
	var raw struct {
		JSONRPC   string          `json:"jsonrpc"`
		ID        json.RawMessage `json:"id"`
		Method    string          `json:"method"`
		Params    json.RawMessage `json:"params"`
		Result    json.RawMessage `json:"result"`
		Error     json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	*m = Message{}
	m.JSONRPC = raw.JSONRPC
	m.Method = raw.Method

	if len(raw.Params) > 0 {
		m.Params = raw.Params
	}
	if len(raw.Result) > 0 {
		m.Result = raw.Result
	}

	// Detect whether "id" was present. encoding/json sets raw.ID to nil when
	// the key is absent, but sets it to the literal bytes `null` when the key
	// is present with a null value.
	if raw.ID != nil {
		m.hasID = true
		if string(raw.ID) != "null" {
			var id ID
			if err := json.Unmarshal(raw.ID, &id); err != nil {
				return fmt.Errorf("unmarshal id: %w", err)
			}
			m.ID = &id
		}
	}

	if len(raw.Error) > 0 && string(raw.Error) != "null" {
		var rpcErr acp.RPCError
		if err := json.Unmarshal(raw.Error, &rpcErr); err != nil {
			return fmt.Errorf("unmarshal error: %w", err)
		}
		m.Error = &rpcErr
	}

	// JSON-RPC 2.0 spec: result and error are mutually exclusive.
	if len(m.Result) > 0 && m.Error != nil {
		return fmt.Errorf("invalid JSON-RPC message: result and error are mutually exclusive")
	}

	return nil
}

// IsRequest returns true if this is a request (has ID and Method).
func (m *Message) IsRequest() bool {
	return m.hasID && m.Method != ""
}

// IsResponse returns true if this is a response (has ID, no Method).
func (m *Message) IsResponse() bool {
	return m.hasID && m.Method == ""
}

// IsNotification returns true if this is a notification (no ID, has Method).
func (m *Message) IsNotification() bool {
	return !m.hasID && m.Method != ""
}

// ID represents a JSON-RPC request ID (int64 or string).
type ID struct {
	num   int64
	str   string
	isStr bool
}

// NewIntID creates a numeric ID.
func NewIntID(n int64) *ID {
	return &ID{num: n}
}

// String returns a string representation for use as a map key.
func (id *ID) String() string {
	if id == nil {
		return ""
	}
	if id.isStr {
		return "s:" + id.str
	}
	return fmt.Sprintf("n:%d", id.num)
}

func (id *ID) MarshalJSON() ([]byte, error) {
	if id.isStr {
		return json.Marshal(id.str)
	}
	return json.Marshal(id.num)
}

func (id *ID) UnmarshalJSON(data []byte) error {
	// Try integer first (most common in practice).
	var n int64
	if err := json.Unmarshal(data, &n); err == nil {
		id.num = n
		id.isStr = false
		return nil
	}
	// Try string.
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		id.str = s
		id.isStr = true
		return nil
	}
	// Reject non-integer numbers (e.g. 1.5) — JSON-RPC 2.0 spec says IDs
	// SHOULD NOT contain fractional parts.
	return fmt.Errorf("invalid JSON-RPC id (must be integer or string): %s", string(data))
}

// NewRequest creates a new JSON-RPC request message.
func NewRequest(id *ID, method string, params any) (*Message, error) {
	msg := &Message{
		JSONRPC: Version,
		ID:      id,
		Method:  method,
		hasID:   true,
	}
	if params != nil {
		data, err := json.Marshal(params)
		if err != nil {
			return nil, fmt.Errorf("marshal params: %w", err)
		}
		msg.Params = data
	}
	return msg, nil
}

// NewNotification creates a new JSON-RPC notification message.
func NewNotification(method string, params any) (*Message, error) {
	msg := &Message{
		JSONRPC: Version,
		Method:  method,
	}
	if params != nil {
		data, err := json.Marshal(params)
		if err != nil {
			return nil, fmt.Errorf("marshal params: %w", err)
		}
		msg.Params = data
	}
	return msg, nil
}

// NewResponse creates a success response.
func NewResponse(id *ID, result any) (*Message, error) {
	msg := &Message{
		JSONRPC: Version,
		ID:      id,
		hasID:   true,
	}
	if result != nil {
		data, err := json.Marshal(result)
		if err != nil {
			return nil, fmt.Errorf("marshal result: %w", err)
		}
		msg.Result = data
	} else {
		msg.Result = json.RawMessage(`{}`)
	}
	return msg, nil
}

// NewErrorResponse creates an error response.
func NewErrorResponse(id *ID, rpcErr *acp.RPCError) *Message {
	return &Message{
		JSONRPC: Version,
		ID:      id,
		Error:   rpcErr,
		hasID:   true,
	}
}
