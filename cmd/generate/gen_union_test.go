package main

import (
	"strings"
	"testing"
)

// generateUnstable is a helper that loads the checked-in unstable schema and
// returns the generated source text, failing the test on error.
func generateUnstable(t *testing.T) string {
	t.Helper()
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
		t.Fatalf("generate: %v", err)
	}
	return string(src)
}

func TestObjectUnionParentSharedFieldsShapes(t *testing.T) {
	text := generateUnstable(t)

	// SetSessionConfigOptionRequest gains the value_id branch; both branches
	// carry parent shared fields; required scalar gets pointer presence.
	for _, expected := range []string{
		"type SetSessionConfigOptionRequestValueID struct {",
		"func (s *SetSessionConfigOptionRequest) AsValueID() (SetSessionConfigOptionRequestValueID, bool)",
		"func NewSetSessionConfigOptionRequestValueID(v SetSessionConfigOptionRequestValueID) SetSessionConfigOptionRequest",
		"Value     *bool",                 // boolean branch required scalar -> pointer
		"Value     *SessionConfigValueID", // value_id branch scalar alias -> pointer
		"SessionID *SessionID",
		"ConfigID  *SessionConfigID",
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("missing fragment: %q", expected)
		}
	}

	// Exactly-one + union Validate are emitted for the affected union.
	for _, expected := range []string{
		"func (s SetSessionConfigOptionRequest) MarshalJSON()",
		"exactly one variant must be set",
		"func (s *SetSessionConfigOptionRequest) Validate() error {",
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("missing fragment: %q", expected)
		}
	}

	// hasKey helper present (structural anyOf decoding).
	if !strings.Contains(text, "func hasKey(raw map[string]json.RawMessage, key string) bool {") {
		t.Fatal("missing hasKey helper")
	}
}

func TestObjectUnionAllowlistFallback(t *testing.T) {
	text := generateUnstable(t)
	// SetSessionConfigOptionRequest is the only allowlisted union: unknown
	// discriminator falls back to ValueID rather than erroring.
	idx := strings.Index(text, "func (s *SetSessionConfigOptionRequest) UnmarshalJSON")
	if idx < 0 {
		t.Fatal("missing SetSessionConfigOptionRequest UnmarshalJSON")
	}
	block := text[idx : idx+1200]
	if strings.Contains(block, "unknown discriminator value") {
		t.Fatal("allowlisted union should not error on unknown discriminator")
	}
	if !strings.Contains(block, "s.ValueID = &val") {
		t.Fatal("allowlisted union should fall back to ValueID default")
	}
}

func TestObjectUnionNonAllowlistedRejectsUnknownDiscriminator(t *testing.T) {
	text := generateUnstable(t)
	// CreateElicitationResponse has no allowlist entry and all-const variants:
	// unknown action must error, missing discriminator must error.
	idx := strings.Index(text, "func (c *CreateElicitationResponse) UnmarshalJSON")
	if idx < 0 {
		t.Fatal("missing CreateElicitationResponse UnmarshalJSON")
	}
	block := text[idx : idx+1400]
	if !strings.Contains(block, "unknown discriminator value") {
		t.Fatal("non-allowlisted union must reject unknown discriminator")
	}
	if !strings.Contains(block, "missing discriminator") {
		t.Fatal("union without default must reject missing discriminator")
	}
}

func TestObjectUnionNonDiscriminatorStructuralMatch(t *testing.T) {
	text := generateUnstable(t)
	// ElicitationFormMode is a non-discriminator anyOf decoded by presence.
	for _, expected := range []string{
		"func (e *ElicitationFormMode) UnmarshalJSON",
		"data does not match any variant",
		"ambiguous union, data matches multiple variants",
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("missing fragment: %q", expected)
		}
	}
	// Existing ref-derived accessor / constructor naming is preserved.
	for _, expected := range []string{
		"func (e *ElicitationFormMode) AsElicitationSessionScope()",
		"func NewElicitationFormModeElicitationSessionScope(v ElicitationFormModeElicitationSessionScope) ElicitationFormMode",
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("missing fragment: %q", expected)
		}
	}
}

func TestSessionUpdateAndAuthMethodUnionValidate(t *testing.T) {
	text := generateUnstable(t)
	// SessionUpdate / AuthMethod keep payload-only constructor signatures.
	for _, expected := range []string{
		"func NewSessionUpdateToolCall(v ToolCall) SessionUpdate",
		"func NewAuthMethodEnvVarVariant(v AuthMethodEnvVar) AuthMethod",
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("missing unchanged fragment: %q", expected)
		}
	}
	// They gain an exactly-one union-level Validate so that a required value-typed
	// union field (e.g. SessionNotification.Update) is no longer a no-op when the
	// union is left zero. The guarded recursion previously emitted in the holder
	// only takes effect once these unions implement Validate().
	for _, expected := range []string{
		"func (s *SessionUpdate) Validate() error {",
		"func (a *AuthMethod) Validate() error {",
		"SessionUpdate: exactly one variant must be set",
		"AuthMethod: exactly one variant must be set",
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("missing union Validate fragment: %q", expected)
		}
	}
	// Each variant wrapper also gains a Validate that recurses into its embedded
	// payload via the guarded interface assertion.
	for _, expected := range []string{
		"func (v *SessionUpdateToolCall) Validate() error {",
		"func (v *AuthMethodEnvVarVariant) Validate() error {",
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("missing variant Validate fragment: %q", expected)
		}
	}
}

// --- Generation-time error paths via synthetic schemas ---

func genFromDefs(defs map[string]*Schema) error {
	gen := NewGenerator(&Schema{Defs: defs}, nil)
	_, err := gen.Generate("acp")
	return err
}

func objSchema(props map[string]*Schema, required ...string) *Schema {
	return &Schema{Type: SchemaType{"object"}, Properties: props, Required: required}
}

func constProp(v string) *Schema {
	return &Schema{Type: SchemaType{"string"}, Const: &ConstValue{Value: []byte(`"` + v + `"`)}}
}

func TestGenFailsOnDefaultVariantMissingTitle(t *testing.T) {
	// Discriminator union with a no-const variant lacking a title.
	defs := map[string]*Schema{
		"U": {
			Type:          SchemaType{"object"},
			Discriminator: &Discriminator{PropertyName: "kind"},
			Properties:    map[string]*Schema{"shared": {Type: SchemaType{"string"}}},
			Required:      []string{"shared"},
			OneOf: []*Schema{
				objSchema(map[string]*Schema{"kind": constProp("a"), "x": {Type: SchemaType{"string"}}}, "kind"),
				objSchema(map[string]*Schema{"y": {Type: SchemaType{"string"}}}), // no const, no title
			},
		},
	}
	err := genFromDefs(defs)
	if err == nil || !strings.Contains(err.Error(), "no usable name") {
		t.Fatalf("expected missing-title error, got %v", err)
	}
}

func TestGenFailsOnRequiredNullableField(t *testing.T) {
	defs := map[string]*Schema{
		"U": {
			Type:          SchemaType{"object"},
			Discriminator: &Discriminator{PropertyName: "kind"},
			Properties: map[string]*Schema{
				"shared": {Type: SchemaType{"string", "null"}}, // required + nullable
			},
			Required: []string{"shared"},
			OneOf: []*Schema{
				objSchema(map[string]*Schema{"kind": constProp("a")}, "kind"),
				objSchema(map[string]*Schema{"kind": constProp("b")}, "kind"),
			},
		},
	}
	err := genFromDefs(defs)
	if err == nil || !strings.Contains(err.Error(), "permits null") {
		t.Fatalf("expected required-nullable error, got %v", err)
	}
}

func TestGenFailsOnArrayVariantWithParentFields(t *testing.T) {
	defs := map[string]*Schema{
		"U": {
			Type:          SchemaType{"object"},
			Discriminator: &Discriminator{PropertyName: "kind"},
			Properties:    map[string]*Schema{"shared": {Type: SchemaType{"string"}}},
			Required:      []string{"shared"},
			OneOf: []*Schema{
				objSchema(map[string]*Schema{"kind": constProp("a")}, "kind"),
				{Type: SchemaType{"array"}, Items: &Schema{Type: SchemaType{"string"}}},
			},
		},
	}
	err := genFromDefs(defs)
	if err == nil || !strings.Contains(err.Error(), "array variant") {
		t.Fatalf("expected array-variant error, got %v", err)
	}
}

func TestGenFailsOnAllowlistMismatch(t *testing.T) {
	// Reuse the real allowlisted name but give it no unique default variant.
	defs := map[string]*Schema{
		"SetSessionConfigOptionRequest": {
			Type:          SchemaType{"object"},
			Discriminator: &Discriminator{PropertyName: "type"},
			Properties:    map[string]*Schema{"sessionId": {Type: SchemaType{"string"}}},
			Required:      []string{"sessionId"},
			OneOf: []*Schema{
				objSchema(map[string]*Schema{"type": constProp("boolean")}, "type"),
				objSchema(map[string]*Schema{"type": constProp("other")}, "type"),
			},
		},
	}
	err := genFromDefs(defs)
	if err == nil || !strings.Contains(err.Error(), "allowlist") {
		t.Fatalf("expected allowlist error, got %v", err)
	}
}

func TestGenFailsOnParentPayloadFieldConflict(t *testing.T) {
	// Parent declares "x" as a string; a variant payload declares "x" as an
	// integer. Same JSON name, incompatible schema -> generation error.
	defs := map[string]*Schema{
		"U": {
			Type:          SchemaType{"object"},
			Discriminator: &Discriminator{PropertyName: "kind"},
			Properties: map[string]*Schema{
				"shared": {Type: SchemaType{"string"}},
				"x":      {Type: SchemaType{"string"}},
			},
			Required: []string{"shared"},
			OneOf: []*Schema{
				objSchema(map[string]*Schema{
					"kind": constProp("a"),
					"x":    {Type: SchemaType{"integer"}},
				}, "kind"),
				objSchema(map[string]*Schema{"kind": constProp("b")}, "kind"),
			},
		},
	}
	err := genFromDefs(defs)
	if err == nil || !strings.Contains(err.Error(), "conflicts with payload field") {
		t.Fatalf("expected parent/payload conflict error, got %v", err)
	}
}

func TestGenAllowsParentPayloadCompatibleField(t *testing.T) {
	// Same JSON name, identical schema and required state -> no error.
	defs := map[string]*Schema{
		"U": {
			Type:          SchemaType{"object"},
			Discriminator: &Discriminator{PropertyName: "kind"},
			Properties: map[string]*Schema{
				"shared": {Type: SchemaType{"string"}},
				"x":      {Type: SchemaType{"string"}},
			},
			Required: []string{"shared"},
			OneOf: []*Schema{
				objSchema(map[string]*Schema{
					"kind": constProp("a"),
					"x":    {Type: SchemaType{"string"}},
				}, "kind"),
				objSchema(map[string]*Schema{"kind": constProp("b")}, "kind"),
			},
		},
	}
	if err := genFromDefs(defs); err != nil {
		t.Fatalf("compatible parent/payload field should not error, got %v", err)
	}
}
