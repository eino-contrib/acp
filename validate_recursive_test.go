package acp

import (
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
		ToolCall:  ToolCallUpdate{},      // missing required toolCallId
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
