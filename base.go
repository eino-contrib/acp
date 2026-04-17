package acp

import (
	"context"
	"fmt"
)

func methodNotSupported(method string) *RPCError {
	return ErrMethodNotFound(method)
}

func notificationNotImplemented(method string) error {
	return fmt.Errorf("notification handler not implemented: %s", method)
}

// BaseAgent provides strict default implementations for the Agent interface.
//
// All request-style methods return method-not-found by default so missing
// capabilities fail loudly instead of silently succeeding with empty results.
// Notification-style methods return explicit errors so missing protocol hooks do
// not silently disappear during integration.
type BaseAgent struct{}

func (BaseAgent) Initialize(context.Context, InitializeRequest) (InitializeResponse, error) {
	return InitializeResponse{}, methodNotSupported(MethodAgentInitialize)
}

func (BaseAgent) Authenticate(context.Context, AuthenticateRequest) (AuthenticateResponse, error) {
	return AuthenticateResponse{}, methodNotSupported(MethodAgentAuthenticate)
}

func (BaseAgent) SessionCancel(context.Context, CancelNotification) error {
	return notificationNotImplemented(MethodAgentSessionCancel)
}

func (BaseAgent) ListSessions(context.Context, ListSessionsRequest) (ListSessionsResponse, error) {
	return ListSessionsResponse{}, methodNotSupported(MethodAgentListSessions)
}

func (BaseAgent) LoadSession(context.Context, LoadSessionRequest) (LoadSessionResponse, error) {
	return LoadSessionResponse{}, methodNotSupported(MethodAgentLoadSession)
}

func (BaseAgent) NewSession(context.Context, NewSessionRequest) (NewSessionResponse, error) {
	return NewSessionResponse{}, methodNotSupported(MethodAgentNewSession)
}

func (BaseAgent) Prompt(context.Context, PromptRequest) (PromptResponse, error) {
	return PromptResponse{}, methodNotSupported(MethodAgentPrompt)
}

func (BaseAgent) SetSessionConfigOption(context.Context, SetSessionConfigOptionRequest) (SetSessionConfigOptionResponse, error) {
	return SetSessionConfigOptionResponse{}, methodNotSupported(MethodAgentSetSessionConfigOption)
}

func (BaseAgent) SetSessionMode(context.Context, SetSessionModeRequest) (SetSessionModeResponse, error) {
	return SetSessionModeResponse{}, methodNotSupported(MethodAgentSetSessionMode)
}

var _ Agent = BaseAgent{}

// BaseClient provides strict default implementations for the Client interface.
//
// Request-style methods return method-not-found by default so client capability
// mismatches are explicit. SessionUpdate also reports an explicit error so UI
// state notifications do not silently disappear when the embedding client has
// not wired the callback yet.
type BaseClient struct{}

func (BaseClient) ReadTextFile(context.Context, ReadTextFileRequest) (ReadTextFileResponse, error) {
	return ReadTextFileResponse{}, methodNotSupported(MethodClientReadTextFile)
}

func (BaseClient) WriteTextFile(context.Context, WriteTextFileRequest) (WriteTextFileResponse, error) {
	return WriteTextFileResponse{}, methodNotSupported(MethodClientWriteTextFile)
}

func (BaseClient) RequestPermission(context.Context, RequestPermissionRequest) (RequestPermissionResponse, error) {
	return RequestPermissionResponse{}, methodNotSupported(MethodClientRequestPermission)
}

func (BaseClient) SessionUpdate(context.Context, SessionNotification) error {
	return notificationNotImplemented(MethodClientSessionUpdate)
}

func (BaseClient) CreateTerminal(context.Context, CreateTerminalRequest) (CreateTerminalResponse, error) {
	return CreateTerminalResponse{}, methodNotSupported(MethodClientCreateTerminal)
}

func (BaseClient) KillTerminal(context.Context, KillTerminalRequest) (KillTerminalResponse, error) {
	return KillTerminalResponse{}, methodNotSupported(MethodClientKillTerminal)
}

func (BaseClient) TerminalOutput(context.Context, TerminalOutputRequest) (TerminalOutputResponse, error) {
	return TerminalOutputResponse{}, methodNotSupported(MethodClientTerminalOutput)
}

func (BaseClient) ReleaseTerminal(context.Context, ReleaseTerminalRequest) (ReleaseTerminalResponse, error) {
	return ReleaseTerminalResponse{}, methodNotSupported(MethodClientReleaseTerminal)
}

func (BaseClient) WaitForTerminalExit(context.Context, WaitForTerminalExitRequest) (WaitForTerminalExitResponse, error) {
	return WaitForTerminalExitResponse{}, methodNotSupported(MethodClientWaitForTerminalExit)
}

var _ Client = BaseClient{}
