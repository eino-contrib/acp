package acp

import (
	"encoding/json"
	"strings"
	"testing"
)

func strp(s string) *string { return &s }
func boolp(b bool) *bool    { return &b }

// --- SetSessionConfigOptionRequest: boolean + value_id branches ---

func TestSetSessionConfigOptionRequestBooleanRoundTrip(t *testing.T) {
	req := NewSetSessionConfigOptionRequestBoolean(SetSessionConfigOptionRequestBoolean{
		Meta:      map[string]any{"k": "v"},
		SessionID: ptrSessionID("s1"),
		ConfigID:  ptrSessionConfigID("c1"),
		Value:     boolp(true),
	})
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err != nil {
		t.Fatalf("unmarshal to map: %v", err)
	}
	for _, k := range []string{"_meta", "sessionId", "configId", "value", "type"} {
		if _, ok := obj[k]; !ok {
			t.Fatalf("boolean branch missing %q in wire: %s", k, data)
		}
	}
	if obj["type"] != "boolean" {
		t.Fatalf("type = %v, want boolean", obj["type"])
	}

	var back SetSessionConfigOptionRequest
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("unmarshal back: %v", err)
	}
	b, ok := back.AsBoolean()
	if !ok {
		t.Fatal("expected boolean branch")
	}
	if b.Value == nil || *b.Value != true {
		t.Fatalf("value = %v, want true", b.Value)
	}
	if b.Meta["k"] != "v" {
		t.Fatalf("_meta not round-tripped: %v", b.Meta)
	}
}

func TestSetSessionConfigOptionRequestValueIDRoundTrip(t *testing.T) {
	v := SessionConfigValueID("vid")
	req := NewSetSessionConfigOptionRequestValueID(SetSessionConfigOptionRequestValueID{
		Meta:      map[string]any{"a": 1.0},
		SessionID: ptrSessionID("s1"),
		ConfigID:  ptrSessionConfigID("c1"),
		Value:     &v,
	})
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var obj map[string]any
	_ = json.Unmarshal(data, &obj)
	if _, ok := obj["type"]; ok {
		t.Fatalf("value_id branch must not write a type field: %s", data)
	}
	for _, k := range []string{"_meta", "sessionId", "configId", "value"} {
		if _, ok := obj[k]; !ok {
			t.Fatalf("value_id branch missing %q: %s", k, data)
		}
	}

	var back SetSessionConfigOptionRequest
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("unmarshal back: %v", err)
	}
	if _, ok := back.AsValueID(); !ok {
		t.Fatal("expected value_id branch")
	}
}

func TestSetSessionConfigOptionRequestMissingTypeFallsToValueID(t *testing.T) {
	// Type field absent, string value payload -> value_id branch.
	var req SetSessionConfigOptionRequest
	in := `{"sessionId":"s1","configId":"c1","value":"vid"}`
	if err := json.Unmarshal([]byte(in), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := req.AsValueID(); !ok {
		t.Fatal("missing type should decode as value_id default")
	}
}

func TestSetSessionConfigOptionRequestUnknownTypeFallsToValueID(t *testing.T) {
	// Unknown type + string value -> allowlist fallback to value_id.
	var req SetSessionConfigOptionRequest
	in := `{"type":"something_new","sessionId":"s1","configId":"c1","value":"vid"}`
	if err := json.Unmarshal([]byte(in), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := req.AsValueID(); !ok {
		t.Fatal("unknown type should fall back to value_id via allowlist")
	}
}

func TestSetSessionConfigOptionRequestValueNonStringFails(t *testing.T) {
	cases := []string{
		`{"sessionId":"s1","configId":"c1","value":123}`,                 // missing type, numeric value
		`{"type":"something_new","sessionId":"s1","configId":"c1","value":123}`, // unknown type, numeric value
	}
	for _, in := range cases {
		var req SetSessionConfigOptionRequest
		if err := json.Unmarshal([]byte(in), &req); err == nil {
			t.Fatalf("expected failure for non-string value: %s", in)
		}
	}
}

func TestSetSessionConfigOptionRequestMissingParentRequiredFails(t *testing.T) {
	cases := map[string]string{
		"missing sessionId": `{"type":"boolean","configId":"c1","value":true}`,
		"missing configId":  `{"type":"boolean","sessionId":"s1","value":true}`,
		"missing value":     `{"type":"boolean","sessionId":"s1","configId":"c1"}`,
	}
	for name, in := range cases {
		var req SetSessionConfigOptionRequest
		if err := json.Unmarshal([]byte(in), &req); err == nil {
			t.Fatalf("%s: expected unmarshal failure, got nil", name)
		}
	}
}

func TestSetSessionConfigOptionRequestBooleanFalseDistinctFromMissing(t *testing.T) {
	// value=false is a legal explicit zero and must succeed.
	var ok SetSessionConfigOptionRequest
	if err := json.Unmarshal([]byte(`{"type":"boolean","sessionId":"s1","configId":"c1","value":false}`), &ok); err != nil {
		t.Fatalf("value=false should be valid: %v", err)
	}
	b, _ := ok.AsBoolean()
	if b.Value == nil || *b.Value != false {
		t.Fatalf("value = %v, want explicit false", b.Value)
	}
	// missing value must fail.
	var bad SetSessionConfigOptionRequest
	if err := json.Unmarshal([]byte(`{"type":"boolean","sessionId":"s1","configId":"c1"}`), &bad); err == nil {
		t.Fatal("missing value should fail")
	}
}

func TestSetSessionConfigOptionRequestNullRequiredFails(t *testing.T) {
	var req SetSessionConfigOptionRequest
	if err := json.Unmarshal([]byte(`{"type":"boolean","sessionId":"s1","configId":"c1","value":null}`), &req); err == nil {
		t.Fatal("required non-nullable value=null should fail")
	}
}

func TestSetSessionConfigOptionRequestValidateExactlyOne(t *testing.T) {
	var none SetSessionConfigOptionRequest
	if err := none.Validate(); err == nil {
		t.Fatal("no variant set should fail Validate")
	}
	if _, err := json.Marshal(none); err == nil {
		t.Fatal("no variant set should fail Marshal")
	}

	both := SetSessionConfigOptionRequest{
		Boolean: &SetSessionConfigOptionRequestBoolean{SessionID: ptrSessionID("s"), ConfigID: ptrSessionConfigID("c"), Value: boolp(true)},
		ValueID: &SetSessionConfigOptionRequestValueID{},
	}
	if err := both.Validate(); err == nil {
		t.Fatal("two variants set should fail Validate")
	}
	if _, err := json.Marshal(both); err == nil {
		t.Fatal("two variants set should fail Marshal")
	}
}

// --- CreateElicitationRequest: parent wrapper + nested union payload ---

func TestCreateElicitationRequestRoundTripPreservesParentAndPayload(t *testing.T) {
	form := ElicitationFormMode{
		ElicitationSessionScope: &ElicitationFormModeElicitationSessionScope{
			ElicitationSessionScope: ElicitationSessionScope{SessionID: "s1"},
			RequestedSchema:         ElicitationSchema{},
		},
	}
	req := NewCreateElicitationRequestForm(CreateElicitationRequestForm{
		ElicitationFormMode: form,
		Meta:                map[string]any{"x": "y"},
		Message:             strp("hello"),
	})
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var obj map[string]any
	_ = json.Unmarshal(data, &obj)
	for _, k := range []string{"_meta", "message", "mode", "requestedSchema", "sessionId"} {
		if _, ok := obj[k]; !ok {
			t.Fatalf("missing %q in flattened wire: %s", k, data)
		}
	}
	if obj["mode"] != "form" {
		t.Fatalf("mode = %v, want form", obj["mode"])
	}

	var back CreateElicitationRequest
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("unmarshal back: %v", err)
	}
	f, ok := back.AsForm()
	if !ok {
		t.Fatal("expected form variant")
	}
	if f.Message == nil || *f.Message != "hello" {
		t.Fatalf("message not round-tripped: %v", f.Message)
	}
	if f.Meta["x"] != "y" {
		t.Fatalf("_meta not round-tripped: %v", f.Meta)
	}
}

func TestCreateElicitationRequestMissingMessageFails(t *testing.T) {
	in := `{"mode":"form","requestedSchema":{},"sessionId":"s1"}`
	var req CreateElicitationRequest
	if err := json.Unmarshal([]byte(in), &req); err == nil {
		t.Fatal("missing parent required message should fail")
	}
}

func TestCreateElicitationRequestMissingPayloadRequiredFailsViaValidate(t *testing.T) {
	// requestedSchema is required on ElicitationFormMode; absent here. The form
	// payload itself (nested union) must surface the failure via recursion.
	req := CreateElicitationRequest{
		Form: &CreateElicitationRequestForm{
			ElicitationFormMode: ElicitationFormMode{}, // no variant set
			Message:             strp("hi"),
		},
	}
	if err := req.Validate(); err == nil {
		t.Fatal("missing nested payload variant should fail recursive Validate")
	}
}

// --- CreateElicitationResponse: optional-only parent (_meta) round-trip ---

func TestCreateElicitationResponseMetaRoundTrip(t *testing.T) {
	resp := NewCreateElicitationResponseDecline(CreateElicitationResponseDecline{
		Meta: map[string]any{"reason": "no"},
	})
	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back CreateElicitationResponse
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	d, ok := back.AsDecline()
	if !ok {
		t.Fatal("expected decline variant")
	}
	if d.Meta["reason"] != "no" {
		t.Fatalf("_meta not round-tripped: %v", d.Meta)
	}
}

func TestCreateElicitationResponseUnknownActionFails(t *testing.T) {
	var resp CreateElicitationResponse
	if err := json.Unmarshal([]byte(`{"action":"bogus"}`), &resp); err == nil {
		t.Fatal("unknown action with no allowlist should fail")
	}
}

// --- ElicitationFormMode / ElicitationUrlMode: non-discriminator anyOf ---

func TestElicitationFormModeStructuralMatch(t *testing.T) {
	// sessionId present -> session scope; requestId present -> request scope.
	var sess ElicitationFormMode
	if err := json.Unmarshal([]byte(`{"requestedSchema":{},"sessionId":"s1"}`), &sess); err != nil {
		t.Fatalf("session decode: %v", err)
	}
	if _, ok := sess.AsElicitationSessionScope(); !ok {
		t.Fatal("expected session scope")
	}

	var reqScope ElicitationFormMode
	if err := json.Unmarshal([]byte(`{"requestedSchema":{},"requestId":"r1"}`), &reqScope); err != nil {
		t.Fatalf("request decode: %v", err)
	}
	if _, ok := reqScope.AsElicitationRequestScope(); !ok {
		t.Fatal("expected request scope")
	}
}

func TestElicitationFormModeNoMatchFails(t *testing.T) {
	var m ElicitationFormMode
	err := json.Unmarshal([]byte(`{"requestedSchema":{}}`), &m)
	if err == nil || !strings.Contains(err.Error(), "does not match any variant") {
		t.Fatalf("expected no-match error, got %v", err)
	}
}

func TestElicitationFormModeMissingParentRequiredFails(t *testing.T) {
	// requestedSchema is parent-required; absent on the wire -> unmarshal fails
	// the presence check before a variant is even matched.
	var m ElicitationFormMode
	if err := json.Unmarshal([]byte(`{"sessionId":"s1"}`), &m); err == nil {
		t.Fatal("missing parent required requestedSchema should fail unmarshal")
	}
}

func TestElicitationUrlModeRequiredFields(t *testing.T) {
	m := ElicitationURLMode{
		ElicitationSessionScope: &ElicitationURLModeElicitationSessionScope{
			ElicitationSessionScope: ElicitationSessionScope{SessionID: "s1"},
		},
	}
	if err := m.Validate(); err == nil {
		t.Fatal("missing elicitationId/url should fail Validate")
	}
}

// --- SessionConfigOption: ref payload + full parent fields ---

func TestSessionConfigOptionSelectRoundTrip(t *testing.T) {
	opt := NewSessionConfigOptionSelect(SessionConfigOptionSelect{
		SessionConfigSelect: SessionConfigSelect{
			CurrentValue: "cur",
			Options:      NewSessionConfigSelectOptionsSessionConfigSelectOptionList([]SessionConfigSelectOption{{Name: "A", Value: "a"}}),
		},
		Meta:        map[string]any{"m": "v"},
		ID:          ptrSessionConfigID("id1"),
		Name:        strp("Name"),
		Description: "desc",
	})
	data, err := json.Marshal(opt)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var obj map[string]any
	_ = json.Unmarshal(data, &obj)
	for _, k := range []string{"_meta", "id", "name", "description", "type", "currentValue"} {
		if _, ok := obj[k]; !ok {
			t.Fatalf("missing %q: %s", k, data)
		}
	}

	var back SessionConfigOption
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	s, ok := back.AsSelect()
	if !ok {
		t.Fatal("expected select variant")
	}
	if s.Meta["m"] != "v" || s.Description != "desc" {
		t.Fatalf("optional parent fields not round-tripped: %+v", s)
	}
}

func TestSessionConfigOptionMissingParentRequiredFails(t *testing.T) {
	cases := map[string]string{
		"missing id":   `{"type":"select","name":"N","currentValue":"c","options":[]}`,
		"missing name": `{"type":"select","id":"id1","currentValue":"c","options":[]}`,
	}
	for name, in := range cases {
		var opt SessionConfigOption
		if err := json.Unmarshal([]byte(in), &opt); err == nil {
			t.Fatalf("%s: expected failure", name)
		}
	}
}

func ptrSessionID(s string) *SessionID {
	v := SessionID(s)
	return &v
}

func ptrSessionConfigID(s string) *SessionConfigID {
	v := SessionConfigID(s)
	return &v
}
