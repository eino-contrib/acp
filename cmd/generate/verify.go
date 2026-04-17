package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
)

// VerifyResult holds the result of verifying generated code against the schema.
type VerifyResult struct {
	SchemaDefsCount    int
	GeneratedTypeCount int
	Missing            []string // $defs names with no corresponding Go type
	AnyFallbacks       []string // schema-backed types that still degraded to `any`
	Extra              []string // Go types with no corresponding $def (expected for union variants)
	SkippedDispatch    []string // intentionally skipped dispatch types
	ClientInterface    *InterfaceVerifyResult
	AgentInterface     *InterfaceVerifyResult
}

// InterfaceVerifyResult holds the result of verifying a generated interface file.
type InterfaceVerifyResult struct {
	InterfaceName     string
	ExpectedMethods   int
	ExpectedConstants int
	MissingMethods    []string
	MissingConstants  []string
	BadSignatures     []string
	ExtraMethods      []string
	ExtraConstants    []string
}

// Verify checks the generated Go file against the schema for completeness.
func Verify(schema *Schema, generatedFile string) (*VerifyResult, error) {
	// 1. Collect all $defs names from schema
	schemaDefs := make(map[string]bool)
	for name := range schema.Defs {
		schemaDefs[name] = true
	}

	// 2. Parse generated Go file to extract type names
	goTypes, anyFallbacks, err := extractGoTypes(generatedFile)
	if err != nil {
		return nil, fmt.Errorf("parsing generated file: %w", err)
	}

	// 3. Build mapping: schema name → expected Go name
	schemaToGo := make(map[string]string)
	for name := range schemaDefs {
		schemaToGo[name] = toTitleCase(name)
	}

	// 4. Find missing types
	var missing []string
	var skipped []string
	for name, goName := range schemaToGo {
		if isDispatchType(name) {
			skipped = append(skipped, name)
			continue
		}
		if !goTypes[goName] {
			missing = append(missing, name+" (expected: "+goName+")")
		}
	}
	sort.Strings(missing)
	sort.Strings(skipped)

	// 5. Find extra types (union variants, etc. - informational)
	expectedGoNames := make(map[string]bool)
	for _, goName := range schemaToGo {
		expectedGoNames[goName] = true
	}
	var extra []string
	for goName := range goTypes {
		if !expectedGoNames[goName] {
			extra = append(extra, goName)
		}
	}
	sort.Strings(extra)

	return &VerifyResult{
		SchemaDefsCount:    len(schemaDefs),
		GeneratedTypeCount: len(goTypes),
		Missing:            missing,
		AnyFallbacks:       anyFallbacks,
		Extra:              extra,
		SkippedDispatch:    skipped,
	}, nil
}

// VerifyInterfaceFile checks the generated interface file contains all expected
// methods and method constants with the correct signatures.
func VerifyInterfaceFile(generatedFile, ifaceName string, expected []MethodInfo) (*InterfaceVerifyResult, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, generatedFile, nil, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("parsing generated interface file: %w", err)
	}

	actualMethods := make(map[string]methodSignature)
	actualConstants := make(map[string]bool)

	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.GenDecl:
			if d.Tok == token.TYPE {
				for _, spec := range d.Specs {
					ts, ok := spec.(*ast.TypeSpec)
					if !ok || ts.Name.Name != ifaceName {
						continue
					}
					iface, ok := ts.Type.(*ast.InterfaceType)
					if !ok {
						return nil, fmt.Errorf("%s is not an interface in %s", ifaceName, generatedFile)
					}
					for _, field := range iface.Methods.List {
						if len(field.Names) == 0 {
							continue
						}
						ft, ok := field.Type.(*ast.FuncType)
						if !ok {
							continue
						}
						actualMethods[field.Names[0].Name] = extractMethodSignature(ft)
					}
				}
			}
			if d.Tok == token.CONST {
				for _, spec := range d.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for _, name := range vs.Names {
						actualConstants[name.Name] = true
					}
				}
			}
		}
	}

	result := &InterfaceVerifyResult{
		InterfaceName:     ifaceName,
		ExpectedMethods:   len(expected),
		ExpectedConstants: len(expected),
	}

	expectedMethodNames := make(map[string]bool, len(expected))
	expectedConstNames := make(map[string]bool, len(expected))
	for _, method := range expected {
		expectedMethodNames[method.GoName] = true
		constName := fmt.Sprintf("Method%s%s", ifaceName, method.GoName)
		expectedConstNames[constName] = true

		actualSig, ok := actualMethods[method.GoName]
		if !ok {
			result.MissingMethods = append(result.MissingMethods, method.GoName)
		} else if err := compareMethodSignature(actualSig, method); err != nil {
			result.BadSignatures = append(result.BadSignatures, fmt.Sprintf("%s: %v", method.GoName, err))
		}

		if !actualConstants[constName] {
			result.MissingConstants = append(result.MissingConstants, constName)
		}
	}

	for name := range actualMethods {
		if !expectedMethodNames[name] {
			result.ExtraMethods = append(result.ExtraMethods, name)
		}
	}
	for name := range actualConstants {
		if strings.HasPrefix(name, "Method"+ifaceName) && !expectedConstNames[name] {
			result.ExtraConstants = append(result.ExtraConstants, name)
		}
	}

	sort.Strings(result.MissingMethods)
	sort.Strings(result.MissingConstants)
	sort.Strings(result.BadSignatures)
	sort.Strings(result.ExtraMethods)
	sort.Strings(result.ExtraConstants)
	return result, nil
}

type methodSignature struct {
	ParamTypes  []string
	ResultTypes []string
}

func extractMethodSignature(ft *ast.FuncType) methodSignature {
	sig := methodSignature{}
	if ft.Params != nil {
		for _, field := range ft.Params.List {
			typeName := exprString(field.Type)
			count := len(field.Names)
			if count == 0 {
				count = 1
			}
			for range count {
				sig.ParamTypes = append(sig.ParamTypes, typeName)
			}
		}
	}
	if ft.Results != nil {
		for _, field := range ft.Results.List {
			typeName := exprString(field.Type)
			count := len(field.Names)
			if count == 0 {
				count = 1
			}
			for range count {
				sig.ResultTypes = append(sig.ResultTypes, typeName)
			}
		}
	}
	return sig
}

func compareMethodSignature(actual methodSignature, expected MethodInfo) error {
	expectedParams := []string{"context.Context", expected.ReqType}
	if !sameStringSlice(actual.ParamTypes, expectedParams) {
		return fmt.Errorf("params = %v, want %v", actual.ParamTypes, expectedParams)
	}

	var expectedResults []string
	if expected.IsNotify {
		expectedResults = []string{"error"}
	} else {
		expectedResults = []string{expected.RespType, "error"}
	}
	if !sameStringSlice(actual.ResultTypes, expectedResults) {
		return fmt.Errorf("results = %v, want %v", actual.ResultTypes, expectedResults)
	}
	return nil
}

func sameStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func exprString(expr ast.Expr) string {
	switch v := expr.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return exprString(v.X) + "." + v.Sel.Name
	case *ast.StarExpr:
		return "*" + exprString(v.X)
	case *ast.ArrayType:
		return "[]" + exprString(v.Elt)
	case *ast.MapType:
		return "map[" + exprString(v.Key) + "]" + exprString(v.Value)
	default:
		return fmt.Sprintf("%T", expr)
	}
}

// extractGoTypes parses a Go source file and returns all top-level type names.
func extractGoTypes(filename string) (map[string]bool, []string, error) {
	src, err := os.ReadFile(filename)
	if err != nil {
		return nil, nil, err
	}

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, parser.ParseComments)
	if err != nil {
		return nil, nil, err
	}

	types := make(map[string]bool)
	var anyFallbacks []string
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.TYPE {
			continue
		}
		for _, spec := range gd.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if ok {
				types[ts.Name.Name] = true
				if ident, ok := ts.Type.(*ast.Ident); ok && ident.Name == "any" {
					anyFallbacks = append(anyFallbacks, ts.Name.Name)
				}
			}
		}
	}
	sort.Strings(anyFallbacks)
	return types, anyFallbacks, nil
}

// PrintVerifyReport prints a human-readable verification report to stderr.
func PrintVerifyReport(r *VerifyResult) {
	fmt.Fprintf(os.Stderr, "\n=== Verification Report ===\n")
	fmt.Fprintf(os.Stderr, "Schema $defs:     %d\n", r.SchemaDefsCount)
	fmt.Fprintf(os.Stderr, "Generated types:  %d\n", r.GeneratedTypeCount)
	fmt.Fprintf(os.Stderr, "Skipped dispatch: %d (%s)\n", len(r.SkippedDispatch), strings.Join(r.SkippedDispatch, ", "))
	fmt.Fprintf(os.Stderr, "Extra types:      %d (union variants etc.)\n", len(r.Extra))

	if len(r.Missing) == 0 {
		fmt.Fprintf(os.Stderr, "Missing types:    0 ✓\n")
	} else {
		fmt.Fprintf(os.Stderr, "Missing types:    %d ✗\n", len(r.Missing))
		for _, m := range r.Missing {
			fmt.Fprintf(os.Stderr, "  - %s\n", m)
		}
	}
	if len(r.AnyFallbacks) == 0 {
		fmt.Fprintf(os.Stderr, "Any fallbacks:    0 ✓\n")
	} else {
		fmt.Fprintf(os.Stderr, "Any fallbacks:    %d ✗\n", len(r.AnyFallbacks))
		for _, m := range r.AnyFallbacks {
			fmt.Fprintf(os.Stderr, "  - %s\n", m)
		}
	}
	printInterfaceVerifyReport(r.ClientInterface)
	printInterfaceVerifyReport(r.AgentInterface)
	fmt.Fprintf(os.Stderr, "===========================\n\n")
}

func printInterfaceVerifyReport(r *InterfaceVerifyResult) {
	if r == nil {
		return
	}
	fmt.Fprintf(os.Stderr, "%s interface methods:   %d\n", r.InterfaceName, r.ExpectedMethods)
	if len(r.MissingMethods) == 0 {
		fmt.Fprintf(os.Stderr, "%s missing methods:   0 ✓\n", r.InterfaceName)
	} else {
		fmt.Fprintf(os.Stderr, "%s missing methods:   %d ✗\n", r.InterfaceName, len(r.MissingMethods))
		for _, item := range r.MissingMethods {
			fmt.Fprintf(os.Stderr, "  - %s\n", item)
		}
	}
	if len(r.MissingConstants) == 0 {
		fmt.Fprintf(os.Stderr, "%s missing consts:    0 ✓\n", r.InterfaceName)
	} else {
		fmt.Fprintf(os.Stderr, "%s missing consts:    %d ✗\n", r.InterfaceName, len(r.MissingConstants))
		for _, item := range r.MissingConstants {
			fmt.Fprintf(os.Stderr, "  - %s\n", item)
		}
	}
	if len(r.BadSignatures) == 0 {
		fmt.Fprintf(os.Stderr, "%s bad signatures:    0 ✓\n", r.InterfaceName)
	} else {
		fmt.Fprintf(os.Stderr, "%s bad signatures:    %d ✗\n", r.InterfaceName, len(r.BadSignatures))
		for _, item := range r.BadSignatures {
			fmt.Fprintf(os.Stderr, "  - %s\n", item)
		}
	}
}
