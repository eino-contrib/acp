package main

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

// validateFuncBody extracts the body of a generated `func (v *Type) Validate()`
// from generated source so tests can assert on the emitted checks.
func validateFuncBody(t *testing.T, src, goType string) string {
	t.Helper()
	re := regexp.MustCompile(`(?s)func \(\w+ \*` + regexp.QuoteMeta(goType) + `\) Validate\(\) error \{(.*?)\n\}`)
	m := re.FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("no Validate() generated for %s", goType)
	}
	return m[1]
}

// TestGenerateValidatesNamedStringRequiredFields guards the fix for required
// fields whose Go type is a named string alias (SessionID, TerminalID,
// AuthMethodID, ...). resolveFieldType maps these schema $ref/allOf properties to
// named types, so a literal goType == "string" check in the validator misses
// them. Each case below is a required string-alias field that must produce a
// non-empty check in the generated Validate().
func TestGenerateValidatesNamedStringRequiredFields(t *testing.T) {
	schema, err := LoadSchema(testFixturePath("schema.unstable.json"))
	if err != nil {
		t.Fatalf("load unstable schema: %v", err)
	}
	meta, err := LoadMeta(testFixturePath("meta.unstable.json"))
	if err != nil {
		t.Fatalf("load unstable meta: %v", err)
	}

	gen := NewGenerator(schema, meta)
	src, err := gen.Generate("acp")
	if err != nil {
		t.Fatalf("generate source: %v", err)
	}
	text := string(src)

	cases := []struct {
		goType   string
		goField  string
		jsonName string
	}{
		{"AuthMethodAgent", "ID", "id"},
		{"AuthenticateRequest", "MethodID", "methodId"},
		{"TerminalOutputRequest", "SessionID", "sessionId"},
		{"TerminalOutputRequest", "TerminalID", "terminalId"},
		{"CreateTerminalResponse", "TerminalID", "terminalId"},
		{"WaitForTerminalExitRequest", "TerminalID", "terminalId"},
		{"PromptRequest", "SessionID", "sessionId"},
		{"SessionNotification", "SessionID", "sessionId"},
		{"ToolCall", "ToolCallID", "toolCallId"},
		{"ConnectMCPResponse", "ConnectionID", "connectionId"},
	}

	for _, tc := range cases {
		body := validateFuncBody(t, text, tc.goType)
		wantCond := "if v." + tc.goField + ` == "" {`
		wantErr := `return fmt.Errorf("` + tc.jsonName + ` is required")`
		if !strings.Contains(body, wantCond) || !strings.Contains(body, wantErr) {
			t.Fatalf("%s.Validate() missing non-empty check for %s; body:\n%s",
				tc.goType, tc.jsonName, body)
		}
	}
}

// TestIsStringLikeExcludesEnumAndConst confirms the resolver does not classify
// enum/const-constrained schemas as plain strings. Those are membership-checked,
// not non-empty-checked, matching the generator's existing behavior.
func TestIsStringLikeExcludesEnumAndConst(t *testing.T) {
	defs := map[string]*Schema{
		"PlainAlias":  {Type: SchemaType{"string"}},
		"EnumAlias":   {Type: SchemaType{"string"}, Enum: rawJSONList(`"a"`, `"b"`)},
		"NumberAlias": {Type: SchemaType{"integer"}},
	}
	g := NewGenerator(&Schema{Defs: defs}, nil)

	stringLike := []*Schema{
		{Type: SchemaType{"string"}},
		{Ref: "#/$defs/PlainAlias"},
		{AllOf: []*Schema{{Ref: "#/$defs/PlainAlias"}}},
		{OneOf: []*Schema{{Type: SchemaType{"null"}}, {Ref: "#/$defs/PlainAlias"}}},
		{Type: SchemaType{"string", "null"}},
	}
	for i, s := range stringLike {
		if !g.isStringLike(s) {
			t.Fatalf("string-like case %d should be string-like", i)
		}
	}

	notStringLike := []*Schema{
		{Ref: "#/$defs/EnumAlias"},
		{AllOf: []*Schema{{Ref: "#/$defs/EnumAlias"}}},
		{Ref: "#/$defs/NumberAlias"},
		{Type: SchemaType{"integer"}},
		{Enum: rawJSONList(`"a"`)},
		{Const: &ConstValue{Value: []byte(`"x"`)}},
		nil,
	}
	for i, s := range notStringLike {
		if g.isStringLike(s) {
			t.Fatalf("non-string-like case %d should not be string-like", i)
		}
	}
}

func rawJSONList(items ...string) []json.RawMessage {
	out := make([]json.RawMessage, 0, len(items))
	for _, it := range items {
		out = append(out, json.RawMessage(it))
	}
	return out
}

// TestGenerateValidateRecursesIntoRequiredNested guards the fix where Validate()
// only checked presence and never descended into a required value-typed struct
// field or the elements of a required slice. The generated RequestPermissionRequest
// must recurse into its required value ToolCall, and Plan must recurse into the
// elements of its required entries slice. The recursion uses a runtime interface
// assertion so it is a safe no-op for non-validatable targets.
func TestGenerateValidateRecursesIntoRequiredNested(t *testing.T) {
	schema, err := LoadSchema(testFixturePath("schema.unstable.json"))
	if err != nil {
		t.Fatalf("load unstable schema: %v", err)
	}
	meta, err := LoadMeta(testFixturePath("meta.unstable.json"))
	if err != nil {
		t.Fatalf("load unstable meta: %v", err)
	}

	gen := NewGenerator(schema, meta)
	src, err := gen.Generate("acp")
	if err != nil {
		t.Fatalf("generate source: %v", err)
	}
	text := string(src)

	// Required value-typed struct field: recurse on its address.
	permBody := validateFuncBody(t, text, "RequestPermissionRequest")
	if !strings.Contains(permBody, "any(&v.ToolCall).(interface{ Validate() error })") {
		t.Fatalf("RequestPermissionRequest.Validate() must recurse into required ToolCall; body:\n%s", permBody)
	}
	// Required slice: recurse per element via &slice[i].
	if !strings.Contains(permBody, "for i := range v.Options {") ||
		!strings.Contains(permBody, "any(&v.Options[i]).(interface{ Validate() error })") {
		t.Fatalf("RequestPermissionRequest.Validate() must recurse into Options elements; body:\n%s", permBody)
	}

	planBody := validateFuncBody(t, text, "Plan")
	if !strings.Contains(planBody, "for i := range v.Entries {") ||
		!strings.Contains(planBody, "any(&v.Entries[i]).(interface{ Validate() error })") {
		t.Fatalf("Plan.Validate() must recurse into Entries elements; body:\n%s", planBody)
	}
}

// TestGenerateValidateDoesNotRecurseIntoPrimitives confirms the recursion is
// gated: required string/enum fields must not gain an interface assertion, so the
// generated code stays free of dead no-op calls on non-composite targets.
func TestGenerateValidateDoesNotRecurseIntoPrimitives(t *testing.T) {
	schema, err := LoadSchema(testFixturePath("schema.unstable.json"))
	if err != nil {
		t.Fatalf("load unstable schema: %v", err)
	}
	meta, err := LoadMeta(testFixturePath("meta.unstable.json"))
	if err != nil {
		t.Fatalf("load unstable meta: %v", err)
	}

	gen := NewGenerator(schema, meta)
	src, err := gen.Generate("acp")
	if err != nil {
		t.Fatalf("generate source: %v", err)
	}
	text := string(src)

	// PlanEntry has only required-string/enum fields; it must keep the plain
	// non-empty check with no nested interface assertion.
	body := validateFuncBody(t, text, "PlanEntry")
	if strings.Contains(body, "interface{ Validate() error }") {
		t.Fatalf("PlanEntry.Validate() should not recurse into primitive fields; body:\n%s", body)
	}
	if !strings.Contains(body, `if v.Content == "" {`) {
		t.Fatalf("PlanEntry.Validate() missing content non-empty check; body:\n%s", body)
	}
}
