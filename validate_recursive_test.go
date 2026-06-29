package acp

import (
	"encoding/json"
	"strings"
	"testing"
)

// These tests cover recursive Validate(): the generated Validate() for a struct
// must descend into required nested struct fields and into the elements of
// required slices, so that a malformed nested value is rejected at the request
// boundary instead of slipping through the top-level presence checks.
//
// Before the fix, RequestPermissionRequest.Validate() only checked options and
// sessionId presence; a zero-value ToolCall (missing the required toolCallId)
// passed validation, and PlanEntry / PermissionOption elements were never
// checked. See gen_validate.go.

func TestValidateRecursesIntoRequiredValueStruct(t *testing.T) {
	// toolCall is a required value-typed ToolCallUpdate. An empty ToolCallUpdate
	// is missing its required toolCallId, so the outer request must fail.
	req := &RequestPermissionRequest{
		SessionID: "s1",
		Options:   []PermissionOption{}, // non-nil: passes presence check
		ToolCall:  ToolCallUpdate{},     // missing required toolCallId
	}
	err := req.Validate()
	if err == nil {
		t.Fatal("expected validation error for empty nested ToolCall, got nil")
	}
	if !strings.Contains(err.Error(), "toolCallId is required") {
		t.Fatalf("error = %q, want it to mention toolCallId", err.Error())
	}
	if !strings.Contains(err.Error(), "ToolCall:") {
		t.Fatalf("error = %q, want nested field context %q", err.Error(), "ToolCall:")
	}
}

func TestValidateAcceptsWellFormedNestedValueStruct(t *testing.T) {
	req := &RequestPermissionRequest{
		SessionID: "s1",
		Options:   []PermissionOption{},
		ToolCall:  ToolCallUpdate{ToolCallID: "tc1"},
	}
	if err := req.Validate(); err != nil {
		t.Fatalf("well-formed request should pass, got: %v", err)
	}
}

func TestValidateRecursesIntoRequiredSliceElements(t *testing.T) {
	// Plan.entries is a required slice. Each PlanEntry requires content; an
	// element with empty content must be rejected with index context.
	plan := &Plan{
		Entries: []PlanEntry{
			{Content: "first", Priority: PlanEntryPriorityHigh, Status: PlanEntryStatusPending},
			{}, // missing required content
		},
	}
	err := plan.Validate()
	if err == nil {
		t.Fatal("expected validation error for invalid plan entry, got nil")
	}
	if !strings.Contains(err.Error(), "content is required") {
		t.Fatalf("error = %q, want it to mention content", err.Error())
	}
	if !strings.Contains(err.Error(), "Entries[1]") {
		t.Fatalf("error = %q, want element index context %q", err.Error(), "Entries[1]")
	}
}

func TestValidateAcceptsWellFormedSliceElements(t *testing.T) {
	plan := &Plan{
		Entries: []PlanEntry{
			{Content: "only", Priority: PlanEntryPriorityHigh, Status: PlanEntryStatusPending},
		},
	}
	if err := plan.Validate(); err != nil {
		t.Fatalf("well-formed plan should pass, got: %v", err)
	}
}

func TestValidateRecursesIntoOptionElements(t *testing.T) {
	// RequestPermissionRequest.options elements are PermissionOption, which
	// requires name and optionId. A populated-but-invalid option element must be
	// rejected, proving the slice recursion runs even when presence passes.
	req := &RequestPermissionRequest{
		SessionID: "s1",
		ToolCall:  ToolCallUpdate{ToolCallID: "tc1"},
		Options: []PermissionOption{
			{Name: "Allow", OptionID: "allow", Kind: PermissionOptionKindAllowOnce},
			{Name: "", OptionID: ""}, // missing required name/optionId
		},
	}
	err := req.Validate()
	if err == nil {
		t.Fatal("expected validation error for invalid option element, got nil")
	}
	if !strings.Contains(err.Error(), "Options[1]") {
		t.Fatalf("error = %q, want element index context %q", err.Error(), "Options[1]")
	}
}

// TestValidateRecursionReachesThroughDispatchBoundary is a focused guard that
// the empty-Validate() generated for purely-nested types is still callable and
// composes through a request that embeds them, so the recursion never panics on
// types without their own required checks.
func TestValidateRecursionHandlesNestedTypeWithoutChecks(t *testing.T) {
	// WriteTextFileRequest has required strings only and no validatable nested
	// struct; it must still validate cleanly when fully populated.
	req := &WriteTextFileRequest{SessionID: "s1", Path: "/tmp/x", Content: "hi"}
	if err := req.Validate(); err != nil {
		t.Fatalf("fully-populated request should pass, got: %v", err)
	}
}

// The tests below guard the fix where a required value-typed discriminated-union
// field used to bypass Validate(). The holder (e.g. SessionNotification.Update,
// RequestPermissionResponse.Outcome) emits a guarded
// any(&v.Field).(interface{ Validate() error }) recursion; before the fix the
// union itself implemented no Validate(), so that assertion failed and the check
// was a silent no-op. A zero-value union (no variant set) — whether built in Go
// or decoded from inbound JSON that omitted the field — now fails validation.

func TestValidateRejectsMissingRequiredUnionField(t *testing.T) {
	// SessionNotification.Update is a required value-typed SessionUpdate union.
	// Left zero, no variant is set, so validation must fail with variant context.
	n := &SessionNotification{SessionID: "s1"}
	err := n.Validate()
	if err == nil {
		t.Fatal("expected validation error for missing required union field, got nil")
	}
	if !strings.Contains(err.Error(), "exactly one variant must be set") {
		t.Fatalf("error = %q, want it to mention the unset union variant", err.Error())
	}
	if !strings.Contains(err.Error(), "Update:") {
		t.Fatalf("error = %q, want nested field context %q", err.Error(), "Update:")
	}
}

func TestValidateRejectsMissingRequiredUnionFieldFromJSON(t *testing.T) {
	// Inbound JSON omitting "update" entirely never triggers SessionUpdate's
	// UnmarshalJSON, so Update stays zero. Validate() must still reject it.
	var n SessionNotification
	if err := json.Unmarshal([]byte(`{"sessionId":"s1"}`), &n); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if err := n.Validate(); err == nil {
		t.Fatal("expected validation error for inbound payload missing 'update', got nil")
	}
}

func TestValidateRejectsMissingRequiredOutcomeUnion(t *testing.T) {
	// RequestPermissionResponse.Outcome is a required value-typed union.
	r := &RequestPermissionResponse{}
	err := r.Validate()
	if err == nil {
		t.Fatal("expected validation error for missing required Outcome union, got nil")
	}
	if !strings.Contains(err.Error(), "exactly one variant must be set") {
		t.Fatalf("error = %q, want it to mention the unset union variant", err.Error())
	}
}

func TestValidateAcceptsWellFormedRequiredUnionField(t *testing.T) {
	// A union with exactly one variant set, whose payload is valid, must pass.
	n := &SessionNotification{
		SessionID: "s1",
		Update:    NewSessionUpdatePlanRemoved(PlanRemoved{ID: "p1"}),
	}
	if err := n.Validate(); err != nil {
		t.Fatalf("well-formed SessionNotification should pass, got: %v", err)
	}
}

func TestValidateRejectsUnionWithInvalidVariantPayload(t *testing.T) {
	// A set variant whose embedded payload is missing a required field must be
	// rejected: the union recurses into the selected variant's Validate().
	// ToolCall.title is present here so the failure is specifically toolCallId.
	n := &SessionNotification{
		SessionID: "s1",
		Update:    NewSessionUpdateToolCall(ToolCall{Title: "t"}), // missing required toolCallId
	}
	err := n.Validate()
	if err == nil {
		t.Fatal("expected validation error for invalid union variant payload, got nil")
	}
	if !strings.Contains(err.Error(), "toolCallId is required") {
		t.Fatalf("error = %q, want it to mention toolCallId", err.Error())
	}
}
