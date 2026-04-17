package jsonrpc

import (
	"encoding/json"
	"testing"

	acptransport "github.com/eino-contrib/acp/transport"
)

func TestParseEnvelopeExtractsPayloadMetadata(t *testing.T) {
	message := json.RawMessage(`{
		"jsonrpc":"2.0",
		"id":1,
		"method":"session/new",
		"params":{"sessionId":"sess-1","protocolVersion":"2025-04-01","nested":{"ok":true}},
		"result":{"sessionId":"sess-2","protocolVersion":"2025-04-02"}
	}`)

	envelope, err := ParseEnvelope(message)
	if err != nil {
		t.Fatalf("ParseEnvelope error = %v", err)
	}

	if envelope.Params.SessionID != "sess-1" {
		t.Fatalf("Params.SessionID = %q, want %q", envelope.Params.SessionID, "sess-1")
	}
	if envelope.Params.ProtocolVersion != "2025-04-01" {
		t.Fatalf("Params.ProtocolVersion = %q, want %q", envelope.Params.ProtocolVersion, "2025-04-01")
	}
	if envelope.Result.SessionID != "sess-2" {
		t.Fatalf("Result.SessionID = %q, want %q", envelope.Result.SessionID, "sess-2")
	}
	if envelope.Result.ProtocolVersion != "2025-04-02" {
		t.Fatalf("Result.ProtocolVersion = %q, want %q", envelope.Result.ProtocolVersion, "2025-04-02")
	}
	if string(envelope.Params.Raw) != `{"sessionId":"sess-1","protocolVersion":"2025-04-01","nested":{"ok":true}}` {
		t.Fatalf("Params.Raw = %s", envelope.Params.Raw)
	}
}

func TestParseEnvelopePreservesNonObjectPayloads(t *testing.T) {
	message := json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":"plain-text-result"}`)

	envelope, err := ParseEnvelope(message)
	if err != nil {
		t.Fatalf("ParseEnvelope error = %v", err)
	}

	if string(envelope.Result.Raw) != `"plain-text-result"` {
		t.Fatalf("Result.Raw = %s", envelope.Result.Raw)
	}
	if envelope.Result.SessionID != "" {
		t.Fatalf("Result.SessionID = %q, want empty", envelope.Result.SessionID)
	}
	if envelope.Result.ProtocolVersion != "" {
		t.Fatalf("Result.ProtocolVersion = %q, want empty", envelope.Result.ProtocolVersion)
	}
}

func TestNormalizeProtocolVersion(t *testing.T) {
	tests := []struct {
		name string
		raw  json.RawMessage
		want string
	}{
		{name: "string", raw: json.RawMessage(`"2025-04-01"`), want: "2025-04-01"},
		{name: "number", raw: json.RawMessage(`2`), want: "2"},
		{name: "null", raw: json.RawMessage(`null`), want: ""},
		{name: "empty", raw: nil, want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := acptransport.NormalizeProtocolVersion(tt.raw); got != tt.want {
				t.Fatalf("NormalizeProtocolVersion(%s) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}
