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
