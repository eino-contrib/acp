package main

import (
	"fmt"
	"strings"
)

// VariantInfo holds information about a discriminated union variant.
type VariantInfo struct {
	ConstValue   string  // discriminator const value (e.g., "text", "agent_message_chunk")
	GoTypeName   string  // Go type name for the variant wrapper (e.g., "ContentBlockText")
	RefTypeName  string  // Referenced Go type name (e.g., "TextContent"), empty if inline
	IsDefault    bool    // True if this is the default variant (no const)
	InlineSchema *Schema // Non-nil if variant has inline properties instead of $ref
}

func (g *Generator) generateDiscriminatedUnion(d Definition) {
	s := d.Schema
	goName := toTitleCase(d.Name)

	// Determine discriminator field
	discField := ""
	if s.Discriminator != nil {
		discField = s.Discriminator.PropertyName
	}

	variants := s.OneOf
	if len(variants) == 0 {
		variants = s.AnyOf
	}

	if discField == "" {
		discField = detectDiscriminator(variants)
	}

	if discField == "" {
		// Fallback: generate as simple union
		g.generateSimpleUnion(d)
		return
	}

	// Extract variant info
	variantInfos := g.extractVariants(goName, discField, variants)
	if len(variantInfos) == 0 {
		g.generateSimpleUnion(d)
		return
	}

	// 1. Generate variant wrapper structs
	for _, vi := range variantInfos {
		if vi.RefTypeName != "" {
			// Variant with embedded ref type
			desc := g.findVariantDescription(variants, vi.ConstValue, discField)
			if desc != "" {
				g.writeComment("", strPtr(desc))
			}
			fmt.Fprintf(&g.buf, "type %s struct {\n", vi.GoTypeName)
			fmt.Fprintf(&g.buf, "\t%s\n", vi.RefTypeName)
			fmt.Fprintf(&g.buf, "\t%s string `json:\"%s\"`\n", toTitleCase(discField), discField)
			fmt.Fprintf(&g.buf, "}\n\n")
		} else if vi.InlineSchema != nil {
			// Inline variant (no ref, has properties)
			desc := g.findVariantDescription(variants, vi.ConstValue, discField)
			if desc != "" {
				g.writeComment("", strPtr(desc))
			}
			fmt.Fprintf(&g.buf, "type %s struct {\n", vi.GoTypeName)
			fmt.Fprintf(&g.buf, "\t%s string `json:\"%s\"`\n", toTitleCase(discField), discField)
			// Inline properties (excluding discriminator field)
			if vi.InlineSchema.Properties != nil {
				inlineSchema := &Schema{
					Properties: make(map[string]*Schema),
					Required:   vi.InlineSchema.Required,
				}
				for k, v := range vi.InlineSchema.Properties {
					if k != discField {
						inlineSchema.Properties[k] = v
					}
				}
				fields := g.buildStructFields(inlineSchema)
				for _, f := range fields {
					fmt.Fprintf(&g.buf, "\t%s %s `json:\"%s\"`\n", f.goName, f.goType, f.jsonTag)
				}
			}
			fmt.Fprintf(&g.buf, "}\n\n")
		}
	}

	// 2. Generate marker interface method on each variant
	markerMethod := fmt.Sprintf("is%sVariant", goName)
	for _, vi := range variantInfos {
		fmt.Fprintf(&g.buf, "func (%s) %s() string {\n", vi.GoTypeName, markerMethod)
		fmt.Fprintf(&g.buf, "\treturn %q\n", vi.ConstValue)
		fmt.Fprintf(&g.buf, "}\n")
	}
	g.buf.WriteString("\n")

	// 3. Generate marker interface
	interfaceName := uncapitalize(goName) + "Variant"
	fmt.Fprintf(&g.buf, "type %s interface{ %s() string }\n\n", interfaceName, markerMethod)

	// 4. Generate union wrapper struct with description
	g.writeComment(goName, s.Description)
	fmt.Fprintf(&g.buf, "type %s struct {\n", goName)
	fmt.Fprintf(&g.buf, "\tvariant %s\n", interfaceName)
	fmt.Fprintf(&g.buf, "}\n\n")

	// 5. Generate MarshalJSON
	recv := receiverName(goName)
	g.generateMarshalJSON(goName, recv, variantInfos, discField)

	// 6. Generate UnmarshalJSON
	g.generateUnmarshalJSON(goName, recv, variantInfos, discField)

	// 7. Generate accessor methods (AsXxx)
	g.generateAccessors(goName, recv, variantInfos)

	// 8. Generate constructor functions (NewXxx)
	g.generateConstructors(goName, variantInfos, discField)
}

func (g *Generator) extractVariants(parentName, discField string, variants []*Schema) []VariantInfo {
	var result []VariantInfo

	for _, v := range variants {
		vi := VariantInfo{}

		// Get discriminator const value
		if v.Properties != nil && v.Properties[discField] != nil {
			if constVal, ok := v.Properties[discField].Const.StringValue(); ok {
				vi.ConstValue = constVal
			}
		}

		// Get ref type
		refType := ""
		if len(v.AllOf) > 0 {
			for _, a := range v.AllOf {
				if a.Ref != "" {
					refType = toTitleCase(resolveRef(a.Ref))
					break
				}
			}
		} else if v.Ref != "" {
			refType = toTitleCase(resolveRef(v.Ref))
		}

		// Build Go type name
		if vi.ConstValue != "" {
			vi.GoTypeName = parentName + toTitleCase(vi.ConstValue)
			// Avoid collision: if wrapper name equals ref type name, use a "Variant" suffix
			if vi.GoTypeName == refType {
				vi.GoTypeName = parentName + toTitleCase(vi.ConstValue) + "Variant"
			}
		} else if refType != "" {
			if strings.HasPrefix(refType, parentName) {
				vi.GoTypeName = refType + "Variant"
			} else {
				vi.GoTypeName = parentName + refType
			}
			vi.IsDefault = true
		} else {
			continue
		}

		if refType != "" {
			vi.RefTypeName = refType
		} else {
			// Inline variant: generate struct even if only discriminator property
			vi.InlineSchema = v
		}

		result = append(result, vi)
	}

	return result
}

func (g *Generator) findVariantDescription(variants []*Schema, constValue, discField string) string {
	for _, v := range variants {
		if v.Properties != nil && v.Properties[discField] != nil {
			if cv, ok := v.Properties[discField].Const.StringValue(); ok && cv == constValue {
				if v.Description != nil {
					return strings.TrimSpace(*v.Description)
				}
			}
		}
	}
	return ""
}

func (g *Generator) generateMarshalJSON(goName, recv string, variants []VariantInfo, discField string) {
	// Check if any variant needs discriminator injection (direct ref without wrapper)
	needsDiscInjection := false
	for _, vi := range variants {
		if vi.IsDefault {
			needsDiscInjection = true
			break
		}
	}

	fmt.Fprintf(&g.buf, "func (%s %s) MarshalJSON() ([]byte, error) {\n", recv, goName)
	fmt.Fprintf(&g.buf, "\tif %s.variant == nil {\n", recv)
	fmt.Fprintf(&g.buf, "\t\treturn nil, fmt.Errorf(\"no variant is set for %s\")\n", goName)
	fmt.Fprintf(&g.buf, "\t}\n")

	if needsDiscInjection {
		// Need to inject discriminator for direct ref variants
		fmt.Fprintf(&g.buf, "\tdata, err := json.Marshal(%s.variant)\n", recv)
		fmt.Fprintf(&g.buf, "\tif err != nil {\n")
		fmt.Fprintf(&g.buf, "\t\treturn nil, err\n")
		fmt.Fprintf(&g.buf, "\t}\n")
		fmt.Fprintf(&g.buf, "\tdisc := %s.variant.is%sVariant()\n", recv, goName)
		fmt.Fprintf(&g.buf, "\tif disc == \"\" {\n")
		fmt.Fprintf(&g.buf, "\t\treturn data, nil\n")
		fmt.Fprintf(&g.buf, "\t}\n")
		fmt.Fprintf(&g.buf, "\tvar obj map[string]json.RawMessage\n")
		fmt.Fprintf(&g.buf, "\tif err := json.Unmarshal(data, &obj); err != nil {\n")
		fmt.Fprintf(&g.buf, "\t\treturn nil, err\n")
		fmt.Fprintf(&g.buf, "\t}\n")
		fmt.Fprintf(&g.buf, "\tobj[%q], _ = json.Marshal(disc)\n", discField)
		fmt.Fprintf(&g.buf, "\treturn json.Marshal(obj)\n")
	} else {
		fmt.Fprintf(&g.buf, "\treturn json.Marshal(%s.variant)\n", recv)
	}

	fmt.Fprintf(&g.buf, "}\n")
}

func (g *Generator) generateUnmarshalJSON(goName, recv string, variants []VariantInfo, discField string) {
	fmt.Fprintf(&g.buf, "func (%s *%s) UnmarshalJSON(data []byte) error {\n", recv, goName)
	fmt.Fprintf(&g.buf, "\tvar disc struct {\n")
	fmt.Fprintf(&g.buf, "\t\t%s string `json:\"%s\"`\n", toTitleCase(discField), discField)
	fmt.Fprintf(&g.buf, "\t}\n")
	fmt.Fprintf(&g.buf, "\tif err := json.Unmarshal(data, &disc); err != nil {\n")
	fmt.Fprintf(&g.buf, "\t\treturn err\n")
	fmt.Fprintf(&g.buf, "\t}\n")
	fmt.Fprintf(&g.buf, "\tswitch disc.%s {\n", toTitleCase(discField))

	hasDefault := false
	discAccessor := fmt.Sprintf("disc.%s", toTitleCase(discField))
	for _, vi := range variants {
		if vi.IsDefault {
			hasDefault = true
			continue
		}
		fmt.Fprintf(&g.buf, "\tcase %q:\n", vi.ConstValue)
		fmt.Fprintf(&g.buf, "\t\tvar v %s\n", vi.GoTypeName)
		fmt.Fprintf(&g.buf, "\t\tif err := json.Unmarshal(data, &v); err != nil {\n")
		fmt.Fprintf(&g.buf, "\t\t\treturn err\n")
		fmt.Fprintf(&g.buf, "\t\t}\n")
		fmt.Fprintf(&g.buf, "\t\t%s.variant = v\n", recv)
		fmt.Fprintf(&g.buf, "\t\treturn nil\n")
	}

	fmt.Fprintf(&g.buf, "\tdefault:\n")
	if hasDefault {
		// Find the default variant
		for _, vi := range variants {
			if vi.IsDefault {
				fmt.Fprintf(&g.buf, "\t\tif %s != \"\" {\n", discAccessor)
				fmt.Fprintf(&g.buf, "\t\t\treturn fmt.Errorf(\"unknown discriminator value: %%s\", %s)\n", discAccessor)
				fmt.Fprintf(&g.buf, "\t\t}\n")
				fmt.Fprintf(&g.buf, "\t\tvar v %s\n", vi.GoTypeName)
				fmt.Fprintf(&g.buf, "\t\tif err := json.Unmarshal(data, &v); err != nil {\n")
				fmt.Fprintf(&g.buf, "\t\t\treturn err\n")
				fmt.Fprintf(&g.buf, "\t\t}\n")
				fmt.Fprintf(&g.buf, "\t\t%s.variant = v\n", recv)
				fmt.Fprintf(&g.buf, "\t\treturn nil\n")
				break
			}
		}
	} else {
		fmt.Fprintf(&g.buf, "\t\treturn fmt.Errorf(\"unknown discriminator value: %%s\", disc.%s)\n", toTitleCase(discField))
	}

	fmt.Fprintf(&g.buf, "\t}\n")
	fmt.Fprintf(&g.buf, "}\n")
}

func (g *Generator) generateAccessors(goName, recv string, variants []VariantInfo) {
	for _, vi := range variants {
		// Accessor name: strip parent prefix to get short name
		shortName := strings.TrimPrefix(vi.GoTypeName, goName)
		if shortName == "" {
			shortName = vi.GoTypeName
		}

		fmt.Fprintf(&g.buf, "func (%s *%s) As%s() (%s, bool) {\n", recv, goName, shortName, vi.GoTypeName)
		fmt.Fprintf(&g.buf, "\tv, ok := %s.variant.(%s)\n", recv, vi.GoTypeName)
		fmt.Fprintf(&g.buf, "\treturn v, ok\n")
		fmt.Fprintf(&g.buf, "}\n")
	}
	g.buf.WriteString("\n")
}

// generateConstructors generates New<Union><Variant> constructor functions for
// each variant of a discriminated union, allowing users to build union values
// programmatically without manual JSON round-tripping.
func (g *Generator) generateConstructors(goName string, variants []VariantInfo, discField string) {
	discFieldGo := toTitleCase(discField)
	for _, vi := range variants {
		shortName := strings.TrimPrefix(vi.GoTypeName, goName)
		if shortName == "" {
			shortName = vi.GoTypeName
		}
		funcName := "New" + goName + shortName

		if vi.RefTypeName != "" {
			// Variant wraps a ref type — constructor takes the ref type as arg.
			fmt.Fprintf(&g.buf, "// %s creates a %s holding a %s variant.\n", funcName, goName, shortName)
			fmt.Fprintf(&g.buf, "func %s(v %s) %s {\n", funcName, vi.RefTypeName, goName)
			fmt.Fprintf(&g.buf, "\treturn %s{variant: %s{\n", goName, vi.GoTypeName)
			fmt.Fprintf(&g.buf, "\t\t%s: v,\n", vi.RefTypeName)
			fmt.Fprintf(&g.buf, "\t\t%s: %q,\n", discFieldGo, vi.ConstValue)
			fmt.Fprintf(&g.buf, "\t}}\n")
			fmt.Fprintf(&g.buf, "}\n\n")
		} else if vi.InlineSchema != nil {
			// Inline variant — constructor takes the wrapper type itself.
			fmt.Fprintf(&g.buf, "// %s creates a %s holding a %s variant.\n", funcName, goName, shortName)
			fmt.Fprintf(&g.buf, "func %s(v %s) %s {\n", funcName, vi.GoTypeName, goName)
			fmt.Fprintf(&g.buf, "\tv.%s = %q\n", discFieldGo, vi.ConstValue)
			fmt.Fprintf(&g.buf, "\treturn %s{variant: v}\n", goName)
			fmt.Fprintf(&g.buf, "}\n\n")
		}
	}
}
