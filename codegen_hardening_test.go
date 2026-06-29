package acp

import (
	"encoding/json"
	"testing"
)

// These tests guard two codegen-hardening fixes made for ACP schema v2:
//
//   - Required fields typed as a string alias (e.g. SessionID, MCPConnectionID,
//     AuthMethodID) must be validated for a non-empty value, just like plain
//     string fields. Previously the validator only recognised the literal Go
//     type "string", so alias-typed required fields silently skipped validation.
//
//   - A union's try-parse fallback must reject a payload that decodes into a
//     variant but does not satisfy that variant's required fields. Previously
//     the first variant that merely json.Unmarshal-ed without error was
//     accepted, so an arbitrary object ({}) was wrongly accepted.

func TestValidateRejectsMissingRequiredStringAlias(t *testing.T) {
	cases := []struct {
		name string
		v    interface{ Validate() error }
	}{
		{"LoginAuthRequest.methodId", &LoginAuthRequest{}},
		{"ConnectMCPRequest.acpId", &ConnectMCPRequest{}},
		{"DisconnectMCPRequest.connectionId", &DisconnectMCPRequest{}},
		// Method is set so the only thing missing is the connectionId alias.
		{"MessageMCPNotification.connectionId", &MessageMCPNotification{Method: "x"}},
		{"MessageMCPRequest.connectionId", &MessageMCPRequest{Method: "x"}},
	}
	for _, tc := range cases {
		if err := tc.v.Validate(); err == nil {
			t.Errorf("%s: Validate() = nil, want error for missing required alias field", tc.name)
		}
	}
}

func TestValidateAcceptsPopulatedStringAlias(t *testing.T) {
	cases := []struct {
		name string
		v    interface{ Validate() error }
	}{
		{"LoginAuthRequest", &LoginAuthRequest{MethodID: "m"}},
		{"ConnectMCPRequest", &ConnectMCPRequest{AcpID: "a"}},
		{"DisconnectMCPRequest", &DisconnectMCPRequest{ConnectionID: "c"}},
		{"MessageMCPNotification", &MessageMCPNotification{ConnectionID: "c", Method: "m"}},
		{"MessageMCPRequest", &MessageMCPRequest{ConnectionID: "c", Method: "m"}},
	}
	for _, tc := range cases {
		if err := tc.v.Validate(); err != nil {
			t.Errorf("%s: Validate() = %v, want nil for fully populated value", tc.name, err)
		}
	}
}

func TestAvailableCommandInputRejectsInvalidObjects(t *testing.T) {
	for _, payload := range []string{`{}`, `{"foo":1}`, `{"bar":"x"}`} {
		var a AvailableCommandInput
		if err := json.Unmarshal([]byte(payload), &a); err == nil {
			t.Errorf("Unmarshal(%s) = nil error, want rejection (unstructured=%v other=%v)",
				payload, a.UnstructuredCommandInput != nil, a.Other != nil)
		}
	}
}

func TestAvailableCommandInputAcceptsValidVariants(t *testing.T) {
	var unstructured AvailableCommandInput
	if err := json.Unmarshal([]byte(`{"hint":"type a path"}`), &unstructured); err != nil {
		t.Fatalf("valid unstructured rejected: %v", err)
	}
	if unstructured.UnstructuredCommandInput == nil || unstructured.UnstructuredCommandInput.Hint != "type a path" {
		t.Fatalf("unstructured variant not parsed: %+v", unstructured)
	}

	var other AvailableCommandInput
	if err := json.Unmarshal([]byte(`{"type":"_custom"}`), &other); err != nil {
		t.Fatalf("valid other rejected: %v", err)
	}
	if other.Other == nil || other.Other.Type != "_custom" {
		t.Fatalf("other variant not parsed: %+v", other)
	}
}

// TestAvailableCommandInputOtherValidate confirms the synthesized inline wrapper
// got a generated Validate() enforcing its required discriminator field.
func TestAvailableCommandInputOtherValidate(t *testing.T) {
	if err := (&AvailableCommandInputOther{}).Validate(); err == nil {
		t.Fatal("AvailableCommandInputOther{}.Validate() = nil, want error for missing type")
	}
	if err := (&AvailableCommandInputOther{Type: "_custom"}).Validate(); err != nil {
		t.Fatalf("AvailableCommandInputOther{Type:...}.Validate() = %v, want nil", err)
	}
}
