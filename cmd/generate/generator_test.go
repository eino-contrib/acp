package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResolveCLIPathsRepoRootDefaults(t *testing.T) {
	repoRoot := t.TempDir()
	writeTestFile(t, filepath.Join(repoRoot, "go.mod"), "module example.com/acp\n")
	writeTestFile(t, filepath.Join(repoRoot, "cmd", "generate", "main.go"), "package main\n")
	writeTestFile(t, filepath.Join(repoRoot, "cmd", "generate", "schema", "schema.json"), "{}")
	writeTestFile(t, filepath.Join(repoRoot, "cmd", "generate", "schema", "meta.json"), "{}")

	schemaPath, metaPath, outputPath := resolveCLIPaths(repoRoot, defaultSchemaPath, defaultMetaPath, defaultOutputPath)

	if got, want := schemaPath, filepath.Join(repoRoot, "cmd", "generate", "schema", "schema.json"); got != want {
		t.Fatalf("schema path = %q, want %q", got, want)
	}
	if got, want := metaPath, filepath.Join(repoRoot, "cmd", "generate", "schema", "meta.json"); got != want {
		t.Fatalf("meta path = %q, want %q", got, want)
	}
	if got, want := outputPath, filepath.Join(repoRoot, "types_gen.go"); got != want {
		t.Fatalf("output path = %q, want %q", got, want)
	}
}

func TestResolveCLIPathsGenerateDirDefaults(t *testing.T) {
	repoRoot := t.TempDir()
	generateDir := filepath.Join(repoRoot, "cmd", "generate")
	writeTestFile(t, filepath.Join(repoRoot, "go.mod"), "module example.com/acp\n")
	writeTestFile(t, filepath.Join(generateDir, "main.go"), "package main\n")
	writeTestFile(t, filepath.Join(generateDir, "schema", "schema.json"), "{}")
	writeTestFile(t, filepath.Join(generateDir, "schema", "meta.json"), "{}")

	schemaPath, metaPath, outputPath := resolveCLIPaths(generateDir, defaultSchemaPath, defaultMetaPath, defaultOutputPath)

	if got, want := schemaPath, filepath.Join(generateDir, "schema", "schema.json"); got != want {
		t.Fatalf("schema path = %q, want %q", got, want)
	}
	if got, want := metaPath, filepath.Join(generateDir, "schema", "meta.json"); got != want {
		t.Fatalf("meta path = %q, want %q", got, want)
	}
	if got, want := outputPath, filepath.Join(repoRoot, "types_gen.go"); got != want {
		t.Fatalf("output path = %q, want %q", got, want)
	}
}

func TestResolveCLIPathsGenerateDirFallsBackToLocalSchema(t *testing.T) {
	repoRoot := t.TempDir()
	generateDir := filepath.Join(repoRoot, "cmd", "generate")
	writeTestFile(t, filepath.Join(repoRoot, "go.mod"), "module example.com/acp\n")
	writeTestFile(t, filepath.Join(generateDir, "main.go"), "package main\n")
	writeTestFile(t, filepath.Join(generateDir, "schema", "schema.json"), "{}")
	writeTestFile(t, filepath.Join(generateDir, "schema", "meta.json"), "{}")

	schemaPath, metaPath, outputPath := resolveCLIPaths(generateDir, defaultSchemaPath, defaultMetaPath, defaultOutputPath)

	if got, want := schemaPath, filepath.Join(generateDir, "schema", "schema.json"); got != want {
		t.Fatalf("schema path = %q, want %q", got, want)
	}
	if got, want := metaPath, filepath.Join(generateDir, "schema", "meta.json"); got != want {
		t.Fatalf("meta path = %q, want %q", got, want)
	}
	if got, want := outputPath, filepath.Join(repoRoot, "types_gen.go"); got != want {
		t.Fatalf("output path = %q, want %q", got, want)
	}
}

func TestResolveCLIPathsKeepsExplicitValues(t *testing.T) {
	cwd := t.TempDir()
	explicitSchema := filepath.Join(cwd, "custom", "schema.json")
	explicitMeta := filepath.Join(cwd, "custom", "meta.json")
	explicitOutput := filepath.Join(cwd, "out", "types_gen.go")

	schemaPath, metaPath, outputPath := resolveCLIPaths(cwd, explicitSchema, explicitMeta, explicitOutput)

	if schemaPath != explicitSchema {
		t.Fatalf("schema path = %q, want %q", schemaPath, explicitSchema)
	}
	if metaPath != explicitMeta {
		t.Fatalf("meta path = %q, want %q", metaPath, explicitMeta)
	}
	if outputPath != explicitOutput {
		t.Fatalf("output path = %q, want %q", outputPath, explicitOutput)
	}
}

func TestDownloadFileTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	dest := filepath.Join(t.TempDir(), "schema.json")
	err := downloadFile(server.URL, dest, 50*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !strings.Contains(err.Error(), "Client.Timeout exceeded") {
		t.Fatalf("downloadFile error = %v, want timeout", err)
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestGenerateCurrentSchemaAvoidsTopLevelAnyFallbacks(t *testing.T) {
	schema, err := LoadSchema(testFixturePath("schema.json"))
	if err != nil {
		t.Fatalf("load schema: %v", err)
	}
	meta, err := LoadMeta(testFixturePath("meta.json"))
	if err != nil {
		t.Fatalf("load meta: %v", err)
	}

	gen := NewGenerator(schema, meta)
	src, err := gen.Generate("acp")
	if err != nil {
		t.Fatalf("generate source: %v", err)
	}
	text := string(src)

	for _, forbidden := range []string{
		"type AgentResponse any",
		"type ClientResponse any",
		"type RequestID any",
	} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("unexpected fallback in generated code: %s", forbidden)
		}
	}

	for _, expected := range []string{
		"type AgentResponse struct {",
		"type ClientResponse struct {",
		"type RequestID struct {",
		"func NewRequestIDString(v string) RequestID",
		"func NewRequestIDInt64(v int64) RequestID",
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("missing generated fragment: %s", expected)
		}
	}
}

func TestCheckedInSchemaFixturesAvailableInGenerateDir(t *testing.T) {
	for _, name := range []string{"schema.json", "meta.json"} {
		fixturePath := localFixturePath(name)
		repoData, err := os.ReadFile(fixturePath)
		if err != nil {
			t.Fatalf("read fixture %s: %v", fixturePath, err)
		}
		if len(bytes.TrimSpace(repoData)) == 0 {
			t.Fatalf("fixture %s is empty", fixturePath)
		}
	}
}

func TestGenerateInterfacesVerifyMethodCoverage(t *testing.T) {
	schema, err := LoadSchema(testFixturePath("schema.json"))
	if err != nil {
		t.Fatalf("load schema: %v", err)
	}
	meta, err := LoadMeta(testFixturePath("meta.json"))
	if err != nil {
		t.Fatalf("load meta: %v", err)
	}

	gen := NewGenerator(schema, meta)
	clientSrc, agentSrc, err := gen.GenerateInterfaces("acp")
	if err != nil {
		t.Fatalf("generate interfaces: %v", err)
	}

	tmpDir := t.TempDir()
	clientFile := filepath.Join(tmpDir, "client_gen.go")
	agentFile := filepath.Join(tmpDir, "agent_gen.go")
	if err := os.WriteFile(clientFile, clientSrc, 0o644); err != nil {
		t.Fatalf("write client file: %v", err)
	}
	if err := os.WriteFile(agentFile, agentSrc, 0o644); err != nil {
		t.Fatalf("write agent file: %v", err)
	}

	clientVerify, err := VerifyInterfaceFile(clientFile, "Client", gen.buildMethods(meta.ClientMethods, "client"))
	if err != nil {
		t.Fatalf("verify client interface: %v", err)
	}
	if len(clientVerify.MissingMethods) > 0 || len(clientVerify.MissingConstants) > 0 || len(clientVerify.BadSignatures) > 0 {
		t.Fatalf("client interface verification failed: %+v", clientVerify)
	}

	agentVerify, err := VerifyInterfaceFile(agentFile, "Agent", gen.buildMethods(meta.AgentMethods, "agent"))
	if err != nil {
		t.Fatalf("verify agent interface: %v", err)
	}
	if len(agentVerify.MissingMethods) > 0 || len(agentVerify.MissingConstants) > 0 || len(agentVerify.BadSignatures) > 0 {
		t.Fatalf("agent interface verification failed: %+v", agentVerify)
	}
}

func TestGenerateMethodMetadataIncludesSessionFlags(t *testing.T) {
	schema, err := LoadSchema(testFixturePath("schema.json"))
	if err != nil {
		t.Fatalf("load schema: %v", err)
	}
	meta, err := LoadMeta(testFixturePath("meta.json"))
	if err != nil {
		t.Fatalf("load meta: %v", err)
	}

	gen := NewGenerator(schema, meta)
	src, err := gen.GenerateMethodMetadata("acp")
	if err != nil {
		t.Fatalf("generate method metadata: %v", err)
	}
	text := string(src)

	for _, expected := range []string{
		`"session/new": {`,
		`SessionCreating:`,
		`"session/prompt": {`,
		`SessionHeaderRequired:`,
		`"session/update": {`,
		`Notification:`,
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("missing generated fragment: %s", expected)
		}
	}
}

func TestGenerateCurrentSchemaRejectsUnknownDefaultDiscriminator(t *testing.T) {
	schema, err := LoadSchema(testFixturePath("schema.json"))
	if err != nil {
		t.Fatalf("load schema: %v", err)
	}
	meta, err := LoadMeta(testFixturePath("meta.json"))
	if err != nil {
		t.Fatalf("load meta: %v", err)
	}

	gen := NewGenerator(schema, meta)
	src, err := gen.Generate("acp")
	if err != nil {
		t.Fatalf("generate source: %v", err)
	}
	text := string(src)

	if !strings.Contains(text, `if disc.Type != "" {`) {
		t.Fatalf("missing unknown discriminator guard in generated MCPServer union")
	}
	if !strings.Contains(text, `return fmt.Errorf("unknown discriminator value: %s", disc.Type)`) {
		t.Fatalf("missing unknown discriminator error in generated MCPServer union")
	}
}

func TestGenerateCurrentSchemaAvoidsDuplicatedDefaultVariantNames(t *testing.T) {
	schema, err := LoadSchema(testFixturePath("schema.json"))
	if err != nil {
		t.Fatalf("load schema: %v", err)
	}
	meta, err := LoadMeta(testFixturePath("meta.json"))
	if err != nil {
		t.Fatalf("load meta: %v", err)
	}

	gen := NewGenerator(schema, meta)
	src, err := gen.Generate("acp")
	if err != nil {
		t.Fatalf("generate source: %v", err)
	}
	text := string(src)

	if strings.Contains(text, "type MCPServerMCPServerStdio struct") {
		t.Fatalf("unexpected duplicated default-variant type name in generated code")
	}
	if !strings.Contains(text, "type MCPServerStdioVariant struct") {
		t.Fatalf("missing expected MCPServer stdio default-variant wrapper")
	}
}

func TestGenerateCurrentSchemaDistinguishesArrayUnionVariants(t *testing.T) {
	schema, err := LoadSchema(testFixturePath("schema.json"))
	if err != nil {
		t.Fatalf("load schema: %v", err)
	}
	meta, err := LoadMeta(testFixturePath("meta.json"))
	if err != nil {
		t.Fatalf("load meta: %v", err)
	}

	gen := NewGenerator(schema, meta)
	src, err := gen.Generate("acp")
	if err != nil {
		t.Fatalf("generate source: %v", err)
	}
	text := string(src)

	if !strings.Contains(text, `var items []map[string]json.RawMessage`) {
		t.Fatalf("missing array-union key inspection logic")
	}
	if !strings.Contains(text, `items[0]["group"]`) {
		t.Fatalf("missing grouped SessionConfigSelectOptions discriminator")
	}
	if strings.Contains(text, "var items []map[string]json.RawMessage\n\tif err := json.Unmarshal(data, &items); err == nil && len(items) > 0 {\n\t}") {
		t.Fatalf("unexpected empty array-union discriminator block")
	}

	var grouped []map[string]any
	if err := json.Unmarshal([]byte(`[ {"group":"g1","name":"Group","options":[{"name":"A","value":"a"}] } ]`), &grouped); err != nil {
		t.Fatalf("sanity check grouped payload: %v", err)
	}
}

func TestGenerateCurrentSchemaAddsSimpleUnionConstructors(t *testing.T) {
	schema, err := LoadSchema(testFixturePath("schema.json"))
	if err != nil {
		t.Fatalf("load schema: %v", err)
	}
	meta, err := LoadMeta(testFixturePath("meta.json"))
	if err != nil {
		t.Fatalf("load meta: %v", err)
	}

	gen := NewGenerator(schema, meta)
	src, err := gen.Generate("acp")
	if err != nil {
		t.Fatalf("generate source: %v", err)
	}
	text := string(src)

	for _, expected := range []string{
		"func NewEmbeddedResourceResourceTextResourceContents(v TextResourceContents) EmbeddedResourceResource",
		"func NewEmbeddedResourceResourceBlobResourceContents(v BlobResourceContents) EmbeddedResourceResource",
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("missing simple-union constructor: %s", expected)
		}
	}
}

func TestGenerateTopLevelArrayDefinition(t *testing.T) {
	schema := &Schema{Defs: map[string]*Schema{
		"Tag": {
			Type: SchemaType{"string"},
		},
		"TagList": {
			Type:  SchemaType{"array"},
			Items: &Schema{Ref: "#/$defs/Tag"},
		},
	}}

	gen := NewGenerator(schema, nil)
	src, err := gen.Generate("acp")
	if err != nil {
		t.Fatalf("generate source: %v", err)
	}
	text := string(src)

	if !strings.Contains(text, "type Tag string") {
		t.Fatalf("missing primitive alias for referenced array element: %s", text)
	}
	if !strings.Contains(text, "type TagList []Tag") {
		t.Fatalf("missing top-level array alias generation: %s", text)
	}
}

func testFixturePath(name string) string {
	repoFixture := repoFixturePath(name)
	if _, err := os.Stat(repoFixture); err == nil {
		return repoFixture
	}
	return localFixturePath(name)
}

func repoFixturePath(name string) string {
	return filepath.Join("..", "..", "cmd", "generate", "schema", name)
}

func localFixturePath(name string) string {
	return filepath.Join("schema", name)
}
