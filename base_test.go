package acp

import (
	"context"
	"testing"
)

func TestBaseAgentImplementsDefaults(t *testing.T) {
	t.Parallel()

	var agent Agent = BaseAgent{}

	methodNotFoundChecks := []struct {
		name string
		call func() error
	}{
		{name: "Initialize", call: func() error { _, err := agent.Initialize(context.Background(), InitializeRequest{}); return err }},
		{name: "LoginAuth", call: func() error { _, err := agent.LoginAuth(context.Background(), LoginAuthRequest{}); return err }},
		{name: "LogoutAuth", call: func() error { _, err := agent.LogoutAuth(context.Background(), LogoutAuthRequest{}); return err }},
		{name: "LoadSession", call: func() error { _, err := agent.LoadSession(context.Background(), LoadSessionRequest{}); return err }},
		{name: "ListSessions", call: func() error { _, err := agent.ListSessions(context.Background(), ListSessionsRequest{}); return err }},
		{name: "NewSession", call: func() error { _, err := agent.NewSession(context.Background(), NewSessionRequest{}); return err }},
		{name: "Prompt", call: func() error { _, err := agent.Prompt(context.Background(), PromptRequest{}); return err }},
		{name: "SetSessionConfigOption", call: func() error {
			_, err := agent.SetSessionConfigOption(context.Background(), SetSessionConfigOptionRequest{})
			return err
		}},
	}

	for _, tc := range methodNotFoundChecks {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); err == nil {
				t.Fatalf("expected %s to report method not found", tc.name)
			} else if rpcErr, ok := err.(*RPCError); !ok || rpcErr.Code != int(ErrorCodeMethodNotFound) {
				t.Fatalf("%s error = %v, want method not found", tc.name, err)
			}
		})
	}

	if err := agent.SessionCancel(context.Background(), CancelNotification{}); err == nil {
		t.Fatal("expected SessionCancel to report unimplemented notification handler")
	} else if got, want := err.Error(), "notification handler not implemented: "+MethodAgentSessionCancel; got != want {
		t.Fatalf("SessionCancel error = %v, want %q", err, want)
	}
}

func TestBaseClientImplementsDefaults(t *testing.T) {
	t.Parallel()

	var client Client = BaseClient{}

	methodNotFoundChecks := []struct {
		name string
		call func() error
	}{
		{name: "RequestPermission", call: func() error {
			_, err := client.RequestPermission(context.Background(), RequestPermissionRequest{})
			return err
		}},
		{name: "UnstableCreateElicitation", call: func() error {
			_, err := client.UnstableCreateElicitation(context.Background(), CreateElicitationRequest{})
			return err
		}},
		{name: "UnstableConnectMCP", call: func() error {
			_, err := client.UnstableConnectMCP(context.Background(), ConnectMCPRequest{})
			return err
		}},
		{name: "UnstableDisconnectMCP", call: func() error {
			_, err := client.UnstableDisconnectMCP(context.Background(), DisconnectMCPRequest{})
			return err
		}},
	}

	for _, tc := range methodNotFoundChecks {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); err == nil {
				t.Fatalf("expected %s to report method not found", tc.name)
			} else if rpcErr, ok := err.(*RPCError); !ok || rpcErr.Code != int(ErrorCodeMethodNotFound) {
				t.Fatalf("%s error = %v, want method not found", tc.name, err)
			}
		})
	}

	if err := client.SessionUpdate(context.Background(), SessionNotification{}); err == nil {
		t.Fatal("expected SessionUpdate to report unimplemented notification handler")
	} else if got, want := err.Error(), "notification handler not implemented: "+MethodClientSessionUpdate; got != want {
		t.Fatalf("SessionUpdate error = %v, want %q", err, want)
	}
}
