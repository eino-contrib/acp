package acp

import (
	"strings"
	"testing"
)

// TestValidateRejectsMissingNamedStringRequired covers the regression where
// required fields typed as named string aliases (SessionID, TerminalID,
// AuthMethodID, ...) were skipped by the generated Validate() because the
// validator only matched the literal Go type "string". Each zero-value request
// below omits a required string-alias field and must be rejected.
func TestValidateRejectsMissingNamedStringRequired(t *testing.T) {
	cases := []struct {
		name    string
		value   interface{ Validate() error }
		wantMsg string
	}{
		{"AuthMethodAgent.id", &AuthMethodAgent{Name: "n"}, "id is required"},
		{"AuthenticateRequest.methodId", &AuthenticateRequest{}, "methodId is required"},
		{"TerminalOutputRequest.sessionId", &TerminalOutputRequest{TerminalID: "t1"}, "sessionId is required"},
		{"TerminalOutputRequest.terminalId", &TerminalOutputRequest{SessionID: "s1"}, "terminalId is required"},
		{"CreateTerminalResponse.terminalId", &CreateTerminalResponse{}, "terminalId is required"},
		{"WaitForTerminalExitRequest.terminalId", &WaitForTerminalExitRequest{SessionID: "s1"}, "terminalId is required"},
		{"SessionNotification.sessionId", &SessionNotification{Update: SessionUpdate{}}, "sessionId is required"},
		{"ToolCall.toolCallId", &ToolCall{Title: "t"}, "toolCallId is required"},
		{"ConnectMCPResponse.connectionId", &ConnectMCPResponse{}, "connectionId is required"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.value.Validate()
			if err == nil {
				t.Fatalf("expected validation error %q, got nil", tc.wantMsg)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Fatalf("error = %q, want it to contain %q", err.Error(), tc.wantMsg)
			}
		})
	}
}

// TestValidateAcceptsPresentNamedStringRequired confirms the new check is a
// presence check, not an over-broad rejection: once the required string-alias
// field is set, Validate() passes (assuming other required fields are present).
func TestValidateAcceptsPresentNamedStringRequired(t *testing.T) {
	if err := (&AuthenticateRequest{MethodID: "m1"}).Validate(); err != nil {
		t.Fatalf("AuthenticateRequest with methodId should pass: %v", err)
	}
	if err := (&TerminalOutputRequest{SessionID: "s1", TerminalID: "t1"}).Validate(); err != nil {
		t.Fatalf("TerminalOutputRequest with both ids should pass: %v", err)
	}
	if err := (&CreateTerminalResponse{TerminalID: "t1"}).Validate(); err != nil {
		t.Fatalf("CreateTerminalResponse with terminalId should pass: %v", err)
	}
}
