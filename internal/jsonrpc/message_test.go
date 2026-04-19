package jsonrpc

import (
	"encoding/json"
	"errors"
	"testing"

	acp "github.com/eino-contrib/acp"
)

func TestMessageUnmarshalPreservesNullIDResponses(t *testing.T) {
	raw := []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"parse error"}}`)

	var msg Message
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatalf("unmarshal message: %v", err)
	}

	if !msg.hasID {
		t.Fatal("expected hasID to be true for id:null response")
	}
	if msg.ID != nil {
		t.Fatalf("expected nil ID for id:null response, got %#v", msg.ID)
	}
	if !msg.IsResponse() {
		t.Fatal("expected id:null message to be classified as a response")
	}
	if msg.IsNotification() {
		t.Fatal("did not expect id:null message to be classified as a notification")
	}
	if msg.IsRequest() {
		t.Fatal("did not expect id:null message to be classified as a request")
	}
}

func TestMessageMarshalRoundTripKeepsNullID(t *testing.T) {
	original := NewErrorResponse(nil, acp.ErrInternalError("boom", nil))

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal message: %v", err)
	}

	var decoded Message
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal round-trip: %v", err)
	}

	if !decoded.hasID || decoded.ID != nil {
		t.Fatalf("round-trip lost null-id semantics: hasID=%v id=%v", decoded.hasID, decoded.ID)
	}
	if !decoded.IsResponse() {
		t.Fatal("expected round-tripped null-id message to remain a response")
	}
}

func TestMessageMarshalRoundTripKeepsInternalErrorOriginData(t *testing.T) {
	original := NewErrorResponse(nil, acp.ErrInternalError("boom", errors.New("dial db failed")))

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal message: %v", err)
	}

	var decoded Message
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal round-trip: %v", err)
	}
	if decoded.Error == nil {
		t.Fatal("expected decoded error")
	}

	var payload struct {
		Error       string `json:"error"`
		OriginError string `json:"originError"`
	}
	if err := json.Unmarshal(decoded.Error.Data, &payload); err != nil {
		t.Fatalf("unmarshal error data: %v", err)
	}
	if payload.Error != "internal error" {
		t.Fatalf("error data.error = %q, want %q", payload.Error, "internal error")
	}
	if payload.OriginError != "dial db failed" {
		t.Fatalf("error data.originError = %q, want %q", payload.OriginError, "dial db failed")
	}
}
