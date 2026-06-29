package main

import (
	"fmt"
	"strings"
)

// generateValidateMethods generates Validate() error methods for struct types
// that have required fields needing runtime checks.
func (g *Generator) generateValidateMethods(defs []Definition) {
	for _, d := range defs {
		if d.Type != TypeStruct {
			continue
		}
		if isDispatchType(d.Name) {
			continue
		}
		s := d.Schema
		if len(s.AllOf) > 0 {
			s = mergeAllOf(s)
		}
		if s.Properties == nil {
			continue
		}
		checks := g.buildValidateChecks(s)
		if len(checks) == 0 {
			// Still generate an empty Validate() for types that are
			// used as request/response params so callers can call it uniformly.
			if isRequestOrResponseType(d.Name) {
				g.generateEmptyValidate(d.Name)
			}
			continue
		}
		g.generateValidateFunc(d.Name, checks)
	}
}

type validateCheck struct {
	fieldGoName string
	jsonName    string
	checkKind   validateKind
}

type validateKind int

const (
	validateNonEmptyString validateKind = iota
	validateNonNilPointer
	validateNonNilSlice
)

func (g *Generator) buildValidateChecks(s *Schema) []validateCheck {
	requiredSet := make(map[string]bool)
	for _, r := range s.Required {
		requiredSet[r] = true
	}

	var checks []validateCheck
	propNames := sortedKeys(s.Properties)

	for _, propName := range propNames {
		if !requiredSet[propName] {
			continue
		}
		if propName == "_meta" {
			continue
		}

		prop := s.Properties[propName]
		goName := toTitleCase(propName)
		goType := g.resolveFieldType(prop, true)

		switch {
		case goType == "string" || g.isStringAliasType(prop):
			// A required field is checked for a non-empty value when it is a
			// plain string or a $ref / allOf-ref to a primitive string alias
			// (e.g. SessionID, MCPConnectionID, AuthMethodID). Alias fields used
			// to be skipped because their Go type is the alias name rather than
			// the literal "string", which let missing required identifiers decode
			// to a zero value and slip past Validate().
			checks = append(checks, validateCheck{
				fieldGoName: goName,
				jsonName:    propName,
				checkKind:   validateNonEmptyString,
			})
		case strings.HasPrefix(goType, "*"):
			checks = append(checks, validateCheck{
				fieldGoName: goName,
				jsonName:    propName,
				checkKind:   validateNonNilPointer,
			})
		case strings.HasPrefix(goType, "[]"):
			checks = append(checks, validateCheck{
				fieldGoName: goName,
				jsonName:    propName,
				checkKind:   validateNonNilSlice,
			})
		}
	}
	return checks
}

func (g *Generator) generateValidateFunc(name string, checks []validateCheck) {
	goName := toTitleCase(name)
	g.needFmt = true
	fmt.Fprintf(&g.buf, "func (v *%s) Validate() error {\n", goName)
	for _, c := range checks {
		switch c.checkKind {
		case validateNonEmptyString:
			fmt.Fprintf(&g.buf, "\tif v.%s == \"\" {\n", c.fieldGoName)
			fmt.Fprintf(&g.buf, "\t\treturn fmt.Errorf(\"%s is required\")\n", c.jsonName)
			fmt.Fprintf(&g.buf, "\t}\n")
		case validateNonNilPointer:
			fmt.Fprintf(&g.buf, "\tif v.%s == nil {\n", c.fieldGoName)
			fmt.Fprintf(&g.buf, "\t\treturn fmt.Errorf(\"%s is required\")\n", c.jsonName)
			fmt.Fprintf(&g.buf, "\t}\n")
		case validateNonNilSlice:
			fmt.Fprintf(&g.buf, "\tif v.%s == nil {\n", c.fieldGoName)
			fmt.Fprintf(&g.buf, "\t\treturn fmt.Errorf(\"%s is required\")\n", c.jsonName)
			fmt.Fprintf(&g.buf, "\t}\n")
		}
	}
	fmt.Fprintf(&g.buf, "\treturn nil\n")
	fmt.Fprintf(&g.buf, "}\n\n")
}

func (g *Generator) generateEmptyValidate(name string) {
	goName := toTitleCase(name)
	fmt.Fprintf(&g.buf, "func (v *%s) Validate() error {\n", goName)
	fmt.Fprintf(&g.buf, "\treturn nil\n")
	fmt.Fprintf(&g.buf, "}\n\n")
}

// generateInlineVariantValidate emits a Validate() for a synthesized inline
// union-variant wrapper. These wrappers are created during union generation and
// are not part of the top-level defs that generateValidateMethods walks, so
// without this they would silently accept any object during the union's
// try-parse fallback. The wrapperName is already a Go type name (not a schema
// def name), so it is used verbatim as the receiver type.
func (g *Generator) generateInlineVariantValidate(wrapperName string, variant *Schema) {
	s := variant
	if len(s.AllOf) > 0 {
		s = mergeAllOf(s)
	}
	if s.Properties == nil {
		return
	}
	checks := g.buildValidateChecks(s)
	if len(checks) == 0 {
		return
	}
	g.needFmt = true
	fmt.Fprintf(&g.buf, "func (v *%s) Validate() error {\n", wrapperName)
	for _, c := range checks {
		fmt.Fprintf(&g.buf, "\tif v.%s == \"\" {\n", c.fieldGoName)
		fmt.Fprintf(&g.buf, "\t\treturn fmt.Errorf(\"%s is required\")\n", c.jsonName)
		fmt.Fprintf(&g.buf, "\t}\n")
	}
	fmt.Fprintf(&g.buf, "\treturn nil\n")
	fmt.Fprintf(&g.buf, "}\n\n")
}

func isRequestOrResponseType(name string) bool {
	return strings.HasSuffix(name, "Request") ||
		strings.HasSuffix(name, "Response") ||
		strings.HasSuffix(name, "Notification")
}

// isStringAliasType reports whether a property is a single $ref / allOf-ref to a
// primitive string alias definition (e.g. SessionId, McpConnectionId). Such
// fields generate a Go field typed as the alias name rather than the literal
// "string", so the plain goType=="string" check misses them. They still carry
// string semantics, so a required one is validated for a non-empty value.
func (g *Generator) isStringAliasType(prop *Schema) bool {
	ref := resolveSingleRef(prop)
	if ref == "" {
		return false
	}
	defs := g.defs()
	if defs == nil {
		return false
	}
	target, ok := defs[resolveRef(ref)]
	if !ok || target == nil {
		return false
	}
	if classifyType(target) != TypePrimitive {
		return false
	}
	return target.Type.Contains("string")
}
