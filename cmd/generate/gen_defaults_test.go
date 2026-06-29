package main

import (
	"regexp"
	"strings"
	"testing"
)

// unmarshalFuncBody extracts the body of a generated
// `func (v *Type) UnmarshalJSON(data []byte) error` from generated source.
func unmarshalFuncBody(t *testing.T, src, goType string) string {
	t.Helper()
	re := regexp.MustCompile(`(?s)func \(\w+ \*` + regexp.QuoteMeta(goType) + `\) UnmarshalJSON\(data \[\]byte\) error \{(.*?)\n\}`)
	m := re.FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("no UnmarshalJSON generated for %s", goType)
	}
	return m[1]
}

// TestSchemaParsesDeserializeExtensions confirms the generator's Schema models
// the x-deserialize-* extensions so downstream generation can act on them.
func TestSchemaParsesDeserializeExtensions(t *testing.T) {
	schema, err := LoadSchema(testFixturePath("schema.unstable.json"))
	if err != nil {
		t.Fatalf("load unstable schema: %v", err)
	}

	mcpReq := schema.Defs["MessageMcpRequest"]
	if mcpReq == nil {
		t.Fatalf("MessageMcpRequest not found in schema")
	}
	params := mcpReq.Properties["params"]
	if params == nil || !params.XDeserializeDefaultOnError() {
		t.Fatalf("MessageMcpRequest.params should be x-deserialize-default-on-error")
	}

	plan := schema.Defs["Plan"]
	if plan == nil {
		t.Fatalf("Plan not found in schema")
	}
	entries := plan.Properties["entries"]
	if entries == nil || !entries.XDeserializeSkipInvalidItems() {
		t.Fatalf("Plan.entries should be x-deserialize-skip-invalid-items")
	}
}

// TestGenerateEmitsTolerantUnmarshalers confirms that fields carrying the
// deserialize-tolerance extensions produce sanitize calls in the generated
// UnmarshalJSON, and that the conformant path is a single strict decode.
func TestGenerateEmitsTolerantUnmarshalers(t *testing.T) {
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

	// default-on-error map/object field -> dropIfUndecodable probe.
	mcpBody := unmarshalFuncBody(t, text, "MessageMCPRequest")
	if !strings.Contains(mcpBody, `dropIfUndecodable[map[string]any](raw, "params")`) {
		t.Fatalf("MessageMCPRequest.UnmarshalJSON missing params drop; body:\n%s", mcpBody)
	}

	// skip-invalid-items array field -> keepDecodableItems probe over element type.
	planBody := unmarshalFuncBody(t, text, "Plan")
	if !strings.Contains(planBody, `keepDecodableItems[PlanEntry](raw, "entries")`) {
		t.Fatalf("Plan.UnmarshalJSON missing entries item-skip; body:\n%s", planBody)
	}

	// The helpers must be emitted exactly once.
	if got := strings.Count(text, "func dropIfUndecodable[T any]"); got != 1 {
		t.Fatalf("dropIfUndecodable helper count = %d, want 1", got)
	}
	if got := strings.Count(text, "func keepDecodableItems[T any]"); got != 1 {
		t.Fatalf("keepDecodableItems helper count = %d, want 1", got)
	}
}

// TestGenerateMergesDefaultsWithTolerance confirms types that have both schema
// defaults and tolerance extensions get a single UnmarshalJSON combining both,
// not two conflicting methods.
func TestGenerateMergesDefaultsWithTolerance(t *testing.T) {
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

	// AuthEnvVar has schema defaults (secret=true, optional=false) and a
	// default-on-error _meta. Exactly one UnmarshalJSON must be generated, and
	// it must contain both the default-application block and the tolerant drop.
	if got := strings.Count(text, "func (v *AuthEnvVar) UnmarshalJSON"); got != 1 {
		t.Fatalf("AuthEnvVar UnmarshalJSON count = %d, want 1", got)
	}
	body := unmarshalFuncBody(t, text, "AuthEnvVar")
	if !strings.Contains(body, `apply default for secret`) {
		t.Fatalf("AuthEnvVar.UnmarshalJSON missing default application; body:\n%s", body)
	}
	if !strings.Contains(body, "strictErr != nil") {
		t.Fatalf("AuthEnvVar.UnmarshalJSON missing strict-failure-gated tolerance; body:\n%s", body)
	}
}
