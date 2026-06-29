package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// assertImportsCoverUsage parses generated Go source and fails if any
// fmt.* / json.* selector is referenced without the corresponding import.
// format.Source (run inside Generate) only reformats syntactically valid code;
// it does not catch a missing import, so this AST-level check is what guards the
// import wiring. The package qualifiers we care about are "fmt" (-> "fmt") and
// "json" (-> "encoding/json").
func assertImportsCoverUsage(t *testing.T, src string) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "types_gen.go", src, parser.AllErrors)
	if err != nil {
		t.Fatalf("generated source does not parse: %v\n%s", err, src)
	}

	imported := make(map[string]bool)
	for _, imp := range file.Imports {
		// Import paths are quoted string literals, e.g. "encoding/json".
		imported[imp.Path.Value] = true
	}

	used := make(map[string]bool)
	ast.Inspect(file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		ident, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		switch ident.Name {
		case "fmt", "json":
			used[ident.Name] = true
		}
		return true
	})

	if used["fmt"] && !imported[`"fmt"`] {
		t.Fatalf("generated code references fmt.* but does not import \"fmt\":\n%s", src)
	}
	if used["json"] && !imported[`"encoding/json"`] {
		t.Fatalf("generated code references json.* but does not import \"encoding/json\":\n%s", src)
	}
}

// TestGenerateImportsCoverLateBoundUsage guards the regression where the import
// block was computed by a pre-scan that ran before most emitters recorded their
// needs. A plain struct whose only import triggers are a required field (Validate
// -> fmt) and a schema default (custom UnmarshalJSON -> encoding/json + fmt) has
// no discriminated union or json.RawMessage field, so the old pre-scan left the
// import block empty while the body still referenced fmt and json. Generation now
// derives imports from usage recorded during body emission, so both must appear.
func TestGenerateImportsCoverLateBoundUsage(t *testing.T) {
	schema := &Schema{Defs: map[string]*Schema{
		// A request struct with a required string (drives Validate -> fmt) and a
		// field carrying a schema default (drives custom UnmarshalJSON -> json+fmt).
		// Nothing here triggers the discriminated-union / json.RawMessage paths the
		// old pre-scan keyed off of.
		"WidgetRequest": {
			Type:     SchemaType{"object"},
			Required: []string{"name"},
			Properties: map[string]*Schema{
				"name":    {Type: SchemaType{"string"}},
				"enabled": {Type: SchemaType{"boolean"}, Default: []byte("true")},
			},
		},
	}}

	gen := NewGenerator(schema, nil)
	src, err := gen.Generate("acp")
	if err != nil {
		t.Fatalf("generate source: %v", err)
	}
	text := string(src)

	// Sanity: the emitters that need the imports actually ran.
	if !strings.Contains(text, "func (v *WidgetRequest) Validate() error {") {
		t.Fatalf("expected Validate() to be generated (fmt user):\n%s", text)
	}
	if !strings.Contains(text, "func (v *WidgetRequest) UnmarshalJSON(data []byte) error {") {
		t.Fatalf("expected custom UnmarshalJSON to be generated (json user):\n%s", text)
	}

	assertImportsCoverUsage(t, text)
}
