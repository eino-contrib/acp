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
	// recurse requests a guarded nested Validate() call on the field (or, for
	// slices, each element). It is set only when the field's target type is a
	// named composite (struct/union) that may carry a Validate() method, so the
	// emitted call is never dead code for primitives, strings, enums, or maps.
	recurse bool
}

type validateKind int

const (
	validateNonEmptyString validateKind = iota
	validateNonNilPointer
	validateNonNilSlice
	// validateNestedValue is a required value-typed struct/union field. It has no
	// presence pre-check (a value struct is never nil); its required-ness is
	// enforced transitively by recursing into the field's own Validate().
	validateNestedValue
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
		case goType == "string" || g.isStringLike(prop):
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
				recurse:     g.schemaValidatable(prop),
			})
		case strings.HasPrefix(goType, "[]"):
			checks = append(checks, validateCheck{
				fieldGoName: goName,
				jsonName:    propName,
				checkKind:   validateNonNilSlice,
				recurse:     g.schemaValidatable(prop.Items),
			})
		default:
			// A required value-typed field. Only recurse when it resolves to a
			// named composite type; primitives, enums, and maps fall through with
			// no check, matching prior behavior.
			if g.schemaValidatable(prop) {
				checks = append(checks, validateCheck{
					fieldGoName: goName,
					jsonName:    propName,
					checkKind:   validateNestedValue,
					recurse:     true,
				})
			}
		}
	}
	return checks
}

// schemaValidatable reports whether a property schema resolves to a named
// definition generated as a composite type (struct or union) that may carry a
// Validate() method. It follows $ref, single-element allOf refs, single non-null
// oneOf/anyOf variants, and ref aliases. Primitives, strings, enums, maps,
// free-form objects, and multi-variant unions (which generate as json.RawMessage)
// are not validatable. The generated recursion uses a runtime interface
// assertion, so a target that turns out not to implement Validate() is a safe
// no-op; this predicate exists to keep that recursion off non-composite fields.
func (g *Generator) schemaValidatable(s *Schema) bool {
	return g.schemaValidatableSeen(s, make(map[string]bool))
}

func (g *Generator) schemaValidatableSeen(s *Schema, seen map[string]bool) bool {
	if s == nil {
		return false
	}

	if ref := resolveSingleRef(s); ref != "" {
		name := resolveRef(ref)
		if seen[name] {
			return false
		}
		seen[name] = true
		if g.schema == nil || g.schema.Defs == nil {
			return false
		}
		target := g.schema.Defs[name]
		if target == nil {
			return false
		}
		switch classifyType(target) {
		case TypeStruct, TypeSimpleUnion, TypeDiscriminatedUnion:
			return true
		case TypeRef:
			return g.schemaValidatableSeen(target, seen)
		default:
			return false
		}
	}

	if variants := unionVariants(s); len(variants) > 0 {
		nonNull := filterNull(variants)
		if len(nonNull) != 1 {
			// Multi-variant unions generate as json.RawMessage (no Validate()).
			return false
		}
		return g.schemaValidatableSeen(nonNull[0], seen)
	}

	return false
}

func (g *Generator) generateValidateFunc(name string, checks []validateCheck) {
	goName := toTitleCase(name)
	g.needFmt = true
	fmt.Fprintf(&g.buf, "func (v *%s) Validate() error {\n", goName)
	g.writeValidateChecks(checks)
	fmt.Fprintf(&g.buf, "\treturn nil\n")
	fmt.Fprintf(&g.buf, "}\n\n")
}

// writeValidateChecks emits the per-field check statements for a Validate()
// body. It assumes the receiver variable is named "v". Callers are responsible
// for the func signature and the trailing "return nil".
func (g *Generator) writeValidateChecks(checks []validateCheck) {
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
			if c.recurse {
				// Pointer is non-nil here; assert on the pointer value directly.
				g.writeNestedValidate(c.fieldGoName, false)
			}
		case validateNonNilSlice:
			fmt.Fprintf(&g.buf, "\tif v.%s == nil {\n", c.fieldGoName)
			fmt.Fprintf(&g.buf, "\t\treturn fmt.Errorf(\"%s is required\")\n", c.jsonName)
			fmt.Fprintf(&g.buf, "\t}\n")
			if c.recurse {
				g.writeNestedSliceValidate(c.fieldGoName)
			}
		case validateNestedValue:
			if c.recurse {
				g.writeNestedValidate(c.fieldGoName, true)
			}
		}
	}
}

// writeNestedValidate emits a guarded recursion into a field's Validate(). When
// addr is true the field is a value type and its address is taken; otherwise the
// field is already a (non-nil) pointer. The runtime interface assertion keeps the
// call a no-op for target types that do not implement Validate().
func (g *Generator) writeNestedValidate(fieldGoName string, addr bool) {
	target := "v." + fieldGoName
	if addr {
		target = "&" + target
	}
	fmt.Fprintf(&g.buf, "\tif validator, ok := any(%s).(interface{ Validate() error }); ok {\n", target)
	fmt.Fprintf(&g.buf, "\t\tif err := validator.Validate(); err != nil {\n")
	fmt.Fprintf(&g.buf, "\t\t\treturn fmt.Errorf(\"%s: %%w\", err)\n", fieldGoName)
	fmt.Fprintf(&g.buf, "\t\t}\n")
	fmt.Fprintf(&g.buf, "\t}\n")
}

// writeNestedSliceValidate emits per-element guarded recursion for a slice field.
// Elements are addressed (&slice[i]) so pointer-receiver Validate() methods are
// found. The index is included in the error context.
func (g *Generator) writeNestedSliceValidate(fieldGoName string) {
	fmt.Fprintf(&g.buf, "\tfor i := range v.%s {\n", fieldGoName)
	fmt.Fprintf(&g.buf, "\t\tif validator, ok := any(&v.%s[i]).(interface{ Validate() error }); ok {\n", fieldGoName)
	fmt.Fprintf(&g.buf, "\t\t\tif err := validator.Validate(); err != nil {\n")
	fmt.Fprintf(&g.buf, "\t\t\t\treturn fmt.Errorf(\"%s[%%d]: %%w\", i, err)\n", fieldGoName)
	fmt.Fprintf(&g.buf, "\t\t\t}\n")
	fmt.Fprintf(&g.buf, "\t\t}\n")
	fmt.Fprintf(&g.buf, "\t}\n")
}

// isStringLike reports whether prop ultimately resolves to a free-form string
// schema, following $ref, single-element allOf refs, and single non-null
// oneOf/anyOf variants. Required fields typed this way generate as named string
// aliases (e.g. SessionID, TerminalID), so a literal goType == "string" check
// misses them. Schemas carrying enum or const are deliberately excluded: those
// are membership-constrained and not subject to a plain non-empty check, matching
// the generator's existing behavior of not validating enum values.
func (g *Generator) isStringLike(prop *Schema) bool {
	return g.isStringLikeSchema(prop, make(map[string]bool))
}

func (g *Generator) isStringLikeSchema(s *Schema, seen map[string]bool) bool {
	if s == nil {
		return false
	}
	if len(s.Enum) > 0 || s.Const != nil {
		return false
	}

	if ref := resolveSingleRef(s); ref != "" {
		name := resolveRef(ref)
		if seen[name] {
			return false
		}
		seen[name] = true
		if g.schema == nil || g.schema.Defs == nil {
			return false
		}
		return g.isStringLikeSchema(g.schema.Defs[name], seen)
	}

	if variants := unionVariants(s); len(variants) > 0 {
		nonNull := filterNull(variants)
		if len(nonNull) != 1 {
			return false
		}
		return g.isStringLikeSchema(nonNull[0], seen)
	}

	if len(s.Type) == 0 {
		return false
	}
	hasString := false
	for _, t := range s.Type {
		if t == "null" {
			continue
		}
		if t != "string" {
			return false
		}
		hasString = true
	}
	return hasString
}

func (g *Generator) generateEmptyValidate(name string) {
	goName := toTitleCase(name)
	fmt.Fprintf(&g.buf, "func (v *%s) Validate() error {\n", goName)
	fmt.Fprintf(&g.buf, "\treturn nil\n")
	fmt.Fprintf(&g.buf, "}\n\n")
}

func isRequestOrResponseType(name string) bool {
	return strings.HasSuffix(name, "Request") ||
		strings.HasSuffix(name, "Response") ||
		strings.HasSuffix(name, "Notification")
}
