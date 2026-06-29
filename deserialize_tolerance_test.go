package acp

import (
	"encoding/json"
	"testing"
)

// These tests cover the x-deserialize-default-on-error and
// x-deserialize-skip-invalid-items schema extensions: a field annotated
// default-on-error must fall back to its default (applied schema default,
// otherwise the Go zero value) when its wire value has the wrong shape, instead
// of failing the whole decode; an array annotated skip-invalid-items must drop
// only the undecodable elements. Well-formed payloads must decode exactly as
// before, with no tolerance applied.

func TestDeserializeDefaultOnError_ParamsWrongType(t *testing.T) {
	// MessageMCPRequest.params is type ["object","null"] with
	// x-deserialize-default-on-error. A non-object value must not fail the
	// decode; params falls back to nil.
	data := []byte(`{"connectionId":"c1","method":"tools/call","params":"not-an-object"}`)
	var req MessageMCPRequest
	if err := json.Unmarshal(data, &req); err != nil {
		t.Fatalf("unmarshal should tolerate wrong-typed params, got error: %v", err)
	}
	if req.ConnectionID != "c1" {
		t.Fatalf("connectionId = %q, want c1", req.ConnectionID)
	}
	if req.Method != "tools/call" {
		t.Fatalf("method = %q, want tools/call", req.Method)
	}
	if req.Params != nil {
		t.Fatalf("params = %v, want nil (defaulted on error)", req.Params)
	}
}

func TestDeserializeDefaultOnError_ParamsWellFormed(t *testing.T) {
	// A well-formed object params must be preserved unchanged.
	data := []byte(`{"connectionId":"c1","method":"m","params":{"a":1,"b":"x"}}`)
	var req MessageMCPRequest
	if err := json.Unmarshal(data, &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if req.Params == nil {
		t.Fatalf("params = nil, want decoded object")
	}
	if got, ok := req.Params["b"].(string); !ok || got != "x" {
		t.Fatalf("params[b] = %v, want x", req.Params["b"])
	}
}

func TestDeserializeDefaultOnError_MetaWrongType(t *testing.T) {
	// _meta is type ["object","null"] with default-on-error across nearly all
	// types. A wrong-typed _meta must be tolerated and fall back to nil while
	// the rest of the object decodes normally.
	data := []byte(`{"sessionId":"s1","path":"/tmp/x","content":"hi","_meta":42}`)
	var req WriteTextFileRequest
	if err := json.Unmarshal(data, &req); err != nil {
		t.Fatalf("unmarshal should tolerate wrong-typed _meta, got error: %v", err)
	}
	if req.SessionID != "s1" || req.Path != "/tmp/x" || req.Content != "hi" {
		t.Fatalf("sibling fields not decoded: %+v", req)
	}
	if req.Meta != nil {
		t.Fatalf("_meta = %v, want nil (defaulted on error)", req.Meta)
	}
}

func TestDeserializeSkipInvalidItems_DropsBadElements(t *testing.T) {
	// Plan.entries is a required array with x-deserialize-skip-invalid-items.
	// A malformed element (here, a non-object) must be dropped while valid
	// entries are retained.
	data := []byte(`{"entries":[` +
		`{"content":"first","priority":"high","status":"pending"},` +
		`"garbage",` +
		`{"content":"second","priority":"low","status":"completed"}` +
		`]}`)
	var plan Plan
	if err := json.Unmarshal(data, &plan); err != nil {
		t.Fatalf("unmarshal should skip invalid items, got error: %v", err)
	}
	if len(plan.Entries) != 2 {
		t.Fatalf("entries len = %d, want 2 (one invalid dropped): %+v", len(plan.Entries), plan.Entries)
	}
	if plan.Entries[0].Content != "first" || plan.Entries[1].Content != "second" {
		t.Fatalf("kept wrong entries: %+v", plan.Entries)
	}
}

func TestDeserializeSkipInvalidItems_AllValidUnchanged(t *testing.T) {
	data := []byte(`{"entries":[` +
		`{"content":"only","priority":"high","status":"pending"}` +
		`]}`)
	var plan Plan
	if err := json.Unmarshal(data, &plan); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(plan.Entries) != 1 || plan.Entries[0].Content != "only" {
		t.Fatalf("well-formed array altered: %+v", plan.Entries)
	}
}

func TestDeserializeSkipInvalidItems_NonArrayDropped(t *testing.T) {
	// content/locations on ToolCallUpdate are ["array","null"] +
	// skip-invalid-items (+ default-on-error). A non-array value must be
	// dropped to nil, not fail the decode.
	data := []byte(`{"toolCallId":"t1","content":"not-an-array"}`)
	var upd ToolCallUpdate
	if err := json.Unmarshal(data, &upd); err != nil {
		t.Fatalf("unmarshal should tolerate non-array content, got error: %v", err)
	}
	if upd.ToolCallID != "t1" {
		t.Fatalf("toolCallId = %q, want t1", upd.ToolCallID)
	}
	if upd.Content != nil {
		t.Fatalf("content = %v, want nil (defaulted on error)", upd.Content)
	}
}

func TestDeserializeDefaultOnError_AppliesSchemaDefault(t *testing.T) {
	// AuthEnvVar.secret has schema default true and is default-on-error. When
	// its wire value is wrong-typed, the field must recover to the schema
	// default (true), not the Go zero value (false). optional defaults to false.
	data := []byte(`{"name":"TOKEN","secret":"not-a-bool"}`)
	var v AuthEnvVar
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("unmarshal should tolerate wrong-typed secret, got error: %v", err)
	}
	if v.Name != "TOKEN" {
		t.Fatalf("name = %q, want TOKEN", v.Name)
	}
	if !v.Secret {
		t.Fatalf("secret = false, want true (schema default applied after drop)")
	}
}

func TestDeserializeDefaultOnError_DefaultsPreservedWhenWellFormed(t *testing.T) {
	// When the value is well-formed, the explicit value wins over the default.
	data := []byte(`{"name":"TOKEN","secret":false,"optional":true}`)
	var v AuthEnvVar
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if v.Secret {
		t.Fatalf("secret = true, want false (explicit value)")
	}
	if !v.Optional {
		t.Fatalf("optional = false, want true (explicit value)")
	}
}

func TestDeserializeDefaultOnError_DefaultsAppliedWhenAbsent(t *testing.T) {
	// Absent default-on-error fields still receive their schema defaults
	// (existing default-unmarshaler behavior must be preserved).
	data := []byte(`{"name":"TOKEN"}`)
	var v AuthEnvVar
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !v.Secret {
		t.Fatalf("secret = false, want true (default for absent field)")
	}
	if v.Optional {
		t.Fatalf("optional = true, want false (default for absent field)")
	}
}

func TestDeserializeTolerance_InvalidJSONStillFails(t *testing.T) {
	// Tolerance only recovers wrong-typed values inside a valid JSON object.
	// Syntactically invalid JSON must still fail.
	var req MessageMCPRequest
	if err := json.Unmarshal([]byte(`{not json`), &req); err == nil {
		t.Fatalf("expected error for invalid JSON, got nil")
	}
}

func TestDeserializeTolerance_NonObjectStillFails(t *testing.T) {
	// A top-level non-object payload cannot be recovered into a struct and must
	// fail rather than silently produce a zero value.
	var req MessageMCPRequest
	if err := json.Unmarshal([]byte(`["a","b"]`), &req); err == nil {
		t.Fatalf("expected error for non-object payload, got nil")
	}
}

func TestDeserializeDefaultOnError_NullPreservedAsAbsent(t *testing.T) {
	// An explicit null for a nullable default-on-error field is valid input
	// (decodes to nil), not an error to recover from.
	data := []byte(`{"connectionId":"c1","method":"m","params":null}`)
	var req MessageMCPRequest
	if err := json.Unmarshal(data, &req); err != nil {
		t.Fatalf("unmarshal of explicit null params: %v", err)
	}
	if req.Params != nil {
		t.Fatalf("params = %v, want nil", req.Params)
	}
}
