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

func unionVariantShortName(goName string, vi VariantInfo) string {
	shortName := strings.TrimPrefix(vi.GoTypeName, goName)
	if shortName == "" {
		return vi.GoTypeName
	}
	return shortName
}

func unionVariantFieldName(goName string, vi VariantInfo) string {
	return unionVariantShortName(goName, vi)
}

func (g *Generator) generateDiscriminatedUnion(d Definition) {
	s := d.Schema
	goName := toTitleCase(d.Name)

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
		g.generateSimpleUnion(d)
		return
	}

	variantInfos := g.extractVariants(goName, discField, variants)
	if len(variantInfos) == 0 {
		g.generateSimpleUnion(d)
		return
	}

	for _, vi := range variantInfos {
		desc := g.findVariantDescription(variants, vi.ConstValue, discField)
		if desc != "" {
			g.writeComment("", strPtr(desc))
		}

		fmt.Fprintf(&g.buf, "type %s struct {\n", vi.GoTypeName)
		if vi.RefTypeName != "" {
			fmt.Fprintf(&g.buf, "\t%s\n", vi.RefTypeName)
		}
		fmt.Fprintf(&g.buf, "\t%s string `json:\"%s,omitempty\"`\n", toTitleCase(discField), discField)
		if vi.InlineSchema != nil && vi.InlineSchema.Properties != nil {
			inlineSchema := &Schema{
				Properties: make(map[string]*Schema),
				Required:   make([]string, 0, len(vi.InlineSchema.Required)),
			}
			for _, required := range vi.InlineSchema.Required {
				if required != discField {
					inlineSchema.Required = append(inlineSchema.Required, required)
				}
			}
			for k, v := range vi.InlineSchema.Properties {
				if k != discField {
					inlineSchema.Properties[k] = v
				}
			}
			fields := g.buildStructFields(inlineSchema)
			for _, f := range fields {
				if f.comment != "" {
					fmt.Fprintf(&g.buf, "\t// %s\n", f.comment)
				}
				fmt.Fprintf(&g.buf, "\t%s %s `json:\"%s\"`\n", f.goName, f.goType, f.jsonTag)
			}
		}
		fmt.Fprintf(&g.buf, "}\n\n")
	}

	g.writeComment(goName, s.Description)
	fmt.Fprintf(&g.buf, "type %s struct {\n", goName)
	for _, vi := range variantInfos {
		desc := g.findVariantDescription(variants, vi.ConstValue, discField)
		if desc != "" {
			for _, line := range strings.Split(desc, "\n") {
				line = strings.TrimSpace(line)
				if line == "" {
					fmt.Fprintf(&g.buf, "\t//\n")
					continue
				}
				fmt.Fprintf(&g.buf, "\t// %s\n", line)
			}
		}
		fmt.Fprintf(&g.buf, "\t%s *%s `json:\"-\"`\n", unionVariantFieldName(goName, vi), vi.GoTypeName)
	}
	fmt.Fprintf(&g.buf, "}\n\n")

	recv := receiverName(goName)
	g.generateMarshalJSON(goName, recv, variantInfos, discField)
	g.generateUnmarshalJSON(goName, recv, variantInfos, discField)
	g.generateAccessors(goName, recv, variantInfos)
	g.generateConstructors(goName, variantInfos, discField)
}

func (g *Generator) extractVariants(parentName, discField string, variants []*Schema) []VariantInfo {
	var result []VariantInfo

	for _, v := range variants {
		vi := VariantInfo{}

		if v.Properties != nil && v.Properties[discField] != nil {
			if constVal, ok := v.Properties[discField].Const.StringValue(); ok {
				vi.ConstValue = constVal
			}
		}

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

		if vi.ConstValue != "" {
			vi.GoTypeName = parentName + toTitleCase(vi.ConstValue)
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
	fmt.Fprintf(&g.buf, "func (%s %s) MarshalJSON() ([]byte, error) {\n", recv, goName)
	for _, vi := range variants {
		fieldName := unionVariantFieldName(goName, vi)
		fmt.Fprintf(&g.buf, "\tif %s.%s != nil {\n", recv, fieldName)
		fmt.Fprintf(&g.buf, "\t\tdata, err := json.Marshal(*%s.%s)\n", recv, fieldName)
		fmt.Fprintf(&g.buf, "\t\tif err != nil {\n")
		fmt.Fprintf(&g.buf, "\t\t\treturn nil, err\n")
		fmt.Fprintf(&g.buf, "\t\t}\n")
		if vi.ConstValue == "" {
			fmt.Fprintf(&g.buf, "\t\treturn data, nil\n")
		} else {
			fmt.Fprintf(&g.buf, "\t\tvar obj map[string]json.RawMessage\n")
			fmt.Fprintf(&g.buf, "\t\tif err := json.Unmarshal(data, &obj); err != nil {\n")
			fmt.Fprintf(&g.buf, "\t\t\treturn nil, err\n")
			fmt.Fprintf(&g.buf, "\t\t}\n")
			fmt.Fprintf(&g.buf, "\t\tobj[%q], _ = json.Marshal(%q)\n", discField, vi.ConstValue)
			fmt.Fprintf(&g.buf, "\t\treturn json.Marshal(obj)\n")
		}
		fmt.Fprintf(&g.buf, "\t}\n")
	}
	fmt.Fprintf(&g.buf, "\treturn nil, fmt.Errorf(\"no variant is set for %s\")\n", goName)
	fmt.Fprintf(&g.buf, "}\n")
}

func (g *Generator) generateUnmarshalJSON(goName, recv string, variants []VariantInfo, discField string) {
	fmt.Fprintf(&g.buf, "func (%s *%s) UnmarshalJSON(data []byte) error {\n", recv, goName)
	fmt.Fprintf(&g.buf, "\t*%s = %s{}\n", recv, goName)
	fmt.Fprintf(&g.buf, "\tvar disc struct {\n")
	fmt.Fprintf(&g.buf, "\t\t%s string `json:\"%s\"`\n", toTitleCase(discField), discField)
	fmt.Fprintf(&g.buf, "\t}\n")
	fmt.Fprintf(&g.buf, "\tif err := json.Unmarshal(data, &disc); err != nil {\n")
	fmt.Fprintf(&g.buf, "\t\treturn err\n")
	fmt.Fprintf(&g.buf, "\t}\n")
	fmt.Fprintf(&g.buf, "\tswitch disc.%s {\n", toTitleCase(discField))

	hasDefault := false
	defaultVariant := VariantInfo{}
	for _, vi := range variants {
		if vi.IsDefault {
			hasDefault = true
			defaultVariant = vi
			continue
		}
		fieldName := unionVariantFieldName(goName, vi)
		fmt.Fprintf(&g.buf, "\tcase %q:\n", vi.ConstValue)
		fmt.Fprintf(&g.buf, "\t\tvar v %s\n", vi.GoTypeName)
		fmt.Fprintf(&g.buf, "\t\tif err := json.Unmarshal(data, &v); err != nil {\n")
		fmt.Fprintf(&g.buf, "\t\t\treturn err\n")
		fmt.Fprintf(&g.buf, "\t\t}\n")
		fmt.Fprintf(&g.buf, "\t\t%s.%s = &v\n", recv, fieldName)
		fmt.Fprintf(&g.buf, "\t\treturn nil\n")
	}

	fmt.Fprintf(&g.buf, "\tdefault:\n")
	if hasDefault {
		fieldName := unionVariantFieldName(goName, defaultVariant)
		fmt.Fprintf(&g.buf, "\t\tif disc.%s != \"\" {\n", toTitleCase(discField))
		fmt.Fprintf(&g.buf, "\t\t\treturn fmt.Errorf(\"unknown discriminator value: %%s\", disc.%s)\n", toTitleCase(discField))
		fmt.Fprintf(&g.buf, "\t\t}\n")
		fmt.Fprintf(&g.buf, "\t\tvar v %s\n", defaultVariant.GoTypeName)
		fmt.Fprintf(&g.buf, "\t\tif err := json.Unmarshal(data, &v); err != nil {\n")
		fmt.Fprintf(&g.buf, "\t\t\treturn err\n")
		fmt.Fprintf(&g.buf, "\t\t}\n")
		fmt.Fprintf(&g.buf, "\t\t%s.%s = &v\n", recv, fieldName)
		fmt.Fprintf(&g.buf, "\t\treturn nil\n")
	} else {
		fmt.Fprintf(&g.buf, "\t\treturn fmt.Errorf(\"unknown discriminator value: %%s\", disc.%s)\n", toTitleCase(discField))
	}
	fmt.Fprintf(&g.buf, "\t}\n")
	fmt.Fprintf(&g.buf, "}\n")
}

func (g *Generator) generateAccessors(goName, recv string, variants []VariantInfo) {
	for _, vi := range variants {
		shortName := unionVariantShortName(goName, vi)
		fieldName := unionVariantFieldName(goName, vi)
		fmt.Fprintf(&g.buf, "func (%s *%s) As%s() (%s, bool) {\n", recv, goName, shortName, vi.GoTypeName)
		fmt.Fprintf(&g.buf, "\tif %s.%s == nil {\n", recv, fieldName)
		fmt.Fprintf(&g.buf, "\t\tvar zero %s\n", vi.GoTypeName)
		fmt.Fprintf(&g.buf, "\t\treturn zero, false\n")
		fmt.Fprintf(&g.buf, "\t}\n")
		fmt.Fprintf(&g.buf, "\treturn *%s.%s, true\n", recv, fieldName)
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
		shortName := unionVariantShortName(goName, vi)
		fieldName := unionVariantFieldName(goName, vi)
		funcName := "New" + goName + shortName

		if vi.RefTypeName != "" {
			fmt.Fprintf(&g.buf, "// %s creates a %s holding a %s variant.\n", funcName, goName, shortName)
			fmt.Fprintf(&g.buf, "func %s(v %s) %s {\n", funcName, vi.RefTypeName, goName)
			fmt.Fprintf(&g.buf, "\tw := %s{\n", vi.GoTypeName)
			fmt.Fprintf(&g.buf, "\t\t%s: v,\n", vi.RefTypeName)
			if vi.ConstValue != "" {
				fmt.Fprintf(&g.buf, "\t\t%s: %q,\n", discFieldGo, vi.ConstValue)
			}
			fmt.Fprintf(&g.buf, "\t}\n")
			fmt.Fprintf(&g.buf, "\treturn %s{%s: &w}\n", goName, fieldName)
			fmt.Fprintf(&g.buf, "}\n\n")
		} else if vi.InlineSchema != nil {
			fmt.Fprintf(&g.buf, "// %s creates a %s holding a %s variant.\n", funcName, goName, shortName)
			fmt.Fprintf(&g.buf, "func %s(v %s) %s {\n", funcName, vi.GoTypeName, goName)
			if vi.ConstValue != "" {
				fmt.Fprintf(&g.buf, "\tv.%s = %q\n", discFieldGo, vi.ConstValue)
			}
			fmt.Fprintf(&g.buf, "\treturn %s{%s: &v}\n", goName, fieldName)
			fmt.Fprintf(&g.buf, "}\n\n")
		}
	}
}
