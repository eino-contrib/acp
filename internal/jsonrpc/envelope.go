package jsonrpc

import (
	"encoding/json"

	acp "github.com/eino-contrib/acp"
	acptransport "github.com/eino-contrib/acp/transport"
)

// InitialBufSize is the initial buffer size for SSE stream scanners.
const InitialBufSize = 64 * 1024 // 64KB

// RawIDToKey converts a raw JSON id value to a stable map key using the
// JSON-RPC ID type for consistency with the rest of the codebase.
func RawIDToKey(raw *json.RawMessage) string {
	if raw == nil {
		return ""
	}
	var id ID
	if err := json.Unmarshal(*raw, &id); err != nil {
		// Fall back to raw string representation.
		return string(*raw)
	}
	return id.String()
}

// EnvelopePayload holds the raw JSON along with extracted metadata.
type EnvelopePayload struct {
	Raw             json.RawMessage
	SessionID       string
	ProtocolVersion string
}

func (p *EnvelopePayload) UnmarshalJSON(data []byte) error {
	// Explicit copy to avoid sharing the underlying buffer with json.Unmarshal.
	p.Raw = make(json.RawMessage, len(data))
	copy(p.Raw, data)
	p.SessionID = ""
	p.ProtocolVersion = ""

	if len(data) == 0 {
		return nil
	}

	var meta struct {
		SessionID       string          `json:"sessionId,omitempty"`
		ProtocolVersion json.RawMessage `json:"protocolVersion,omitempty"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		// params/result may be non-object payloads. Keep the raw JSON available
		// and skip metadata extraction in that case.
		return nil
	}

	p.SessionID = meta.SessionID
	p.ProtocolVersion = acptransport.NormalizeProtocolVersion(meta.ProtocolVersion)
	return nil
}

// Envelope is a lightweight JSON-RPC envelope used for message classification
// and metadata extraction without full deserialization.
type Envelope struct {
	JSONRPC string           `json:"jsonrpc,omitempty"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Params  EnvelopePayload  `json:"params,omitempty"`
	Result  EnvelopePayload  `json:"result,omitempty"`
	Error   *acp.RPCError    `json:"error,omitempty"`
}

// ParseEnvelope parses the JSON-RPC envelope from raw bytes.
func ParseEnvelope(data []byte) (Envelope, error) {
	var envelope Envelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return Envelope{}, err
	}
	return envelope, nil
}

// recoverRequestID extracts the "id" field from a syntactically valid but
// semantically invalid JSON-RPC frame so that error responses can echo it
// back to the peer. Returns nil when the id is missing, null, or not a
// valid JSON-RPC id (integer or string) — in which case the spec requires
// replying with id:null.
func recoverRequestID(data []byte) *ID {
	var probe struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil
	}
	if len(probe.ID) == 0 || string(probe.ID) == "null" {
		return nil
	}
	var id ID
	if err := json.Unmarshal(probe.ID, &id); err != nil {
		return nil
	}
	return &id
}

// IsRequest reports whether the envelope represents a JSON-RPC request.
func (e Envelope) IsRequest() bool {
	return e.ID != nil && e.Method != ""
}

// IsNotification reports whether the envelope represents a JSON-RPC notification.
func (e Envelope) IsNotification() bool {
	return e.ID == nil && e.Method != ""
}

// IsResponse reports whether the envelope represents a JSON-RPC response.
func (e Envelope) IsResponse() bool {
	return e.ID != nil && e.Method == ""
}
