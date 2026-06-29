package main

import (
	"encoding/json"
	"fmt"
)

// generateCustomUnmarshalers generates UnmarshalJSON methods for struct types
// that need custom decode behavior: schema "default" values, and the
// x-deserialize-default-on-error / x-deserialize-skip-invalid-items deserialize
// extensions.
//
// The conformant hot path stays a single strict json.Unmarshal. Tolerant
// recovery (sanitizing wrong-typed annotated fields) only runs when the strict
// decode fails, so well-formed payloads keep their existing decode cost and
// behavior.
func (g *Generator) generateCustomUnmarshalers(defs []Definition) {
	needsDropHelper := false
	needsKeepHelper := false

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
		defaults := g.collectDefaults(s)
		tolerant := g.collectTolerantFields(s)
		if len(defaults) == 0 && len(tolerant) == 0 {
			continue
		}
		for _, tf := range tolerant {
			if tf.skipInvalidItems {
				needsKeepHelper = true
			} else {
				needsDropHelper = true
			}
		}
		g.generateCustomUnmarshalJSON(d.Name, defaults, tolerant)
	}

	if needsDropHelper {
		g.generateDropIfUndecodableHelper()
	}
	if needsKeepHelper {
		g.generateKeepDecodableItemsHelper()
	}
}

type defaultEntry struct {
	jsonName string
	goName   string
	rawJSON  string // compact JSON string for the default value
}

// tolerantField describes a property carrying a deserialize-tolerance extension.
type tolerantField struct {
	jsonName         string
	probeType        string // Go type used to test decodability of the value (or array element)
	skipInvalidItems bool   // x-deserialize-skip-invalid-items (always also default-on-error)
}

func (g *Generator) collectDefaults(s *Schema) []defaultEntry {
	var entries []defaultEntry
	propNames := sortedKeys(s.Properties)

	for _, propName := range propNames {
		prop := s.Properties[propName]
		if prop == nil || len(prop.Default) == 0 {
			continue
		}

		// Compact the default value JSON
		var compact json.RawMessage
		if err := json.Unmarshal(prop.Default, &compact); err != nil {
			continue
		}
		compactBytes, err := json.Marshal(compact)
		if err != nil {
			continue
		}

		goName := toTitleCase(propName)
		if propName == "_meta" {
			goName = "Meta"
		}

		entries = append(entries, defaultEntry{
			jsonName: propName,
			goName:   goName,
			rawJSON:  string(compactBytes),
		})
	}
	return entries
}

// collectTolerantFields returns the properties carrying x-deserialize-* tolerance
// extensions, in deterministic order. Fields whose Go type can never fail to
// decode (any / json.RawMessage) are skipped for the default-on-error case since
// the sanitize branch would be a guaranteed no-op.
func (g *Generator) collectTolerantFields(s *Schema) []tolerantField {
	requiredSet := make(map[string]bool)
	for _, r := range s.Required {
		requiredSet[r] = true
	}

	var fields []tolerantField
	for _, propName := range sortedKeys(s.Properties) {
		prop := s.Properties[propName]
		if prop == nil {
			continue
		}
		if prop.XDeserializeSkipInvalidItems() {
			fields = append(fields, tolerantField{
				jsonName:         propName,
				probeType:        g.itemProbeType(prop),
				skipInvalidItems: true,
			})
			continue
		}
		if prop.XDeserializeDefaultOnError() {
			probe := g.tolerantFieldGoType(propName, prop, requiredSet[propName])
			if probe == "any" || probe == "json.RawMessage" {
				continue
			}
			fields = append(fields, tolerantField{
				jsonName:  propName,
				probeType: probe,
			})
		}
	}
	return fields
}

// tolerantFieldGoType returns the struct field Go type for a property, matching
// buildStructFields so the decodability probe targets the same type the Alias
// decodes into.
func (g *Generator) tolerantFieldGoType(propName string, prop *Schema, required bool) string {
	if propName == "_meta" {
		return "map[string]any"
	}
	return g.resolveFieldType(prop, required)
}

// itemProbeType returns the Go type of an array's element, used to test
// individual elements for x-deserialize-skip-invalid-items.
func (g *Generator) itemProbeType(prop *Schema) string {
	if prop.Items != nil {
		return g.resolveFieldType(prop.Items, true)
	}
	return "any"
}

func (g *Generator) generateCustomUnmarshalJSON(name string, defaults []defaultEntry, tolerant []tolerantField) {
	goName := toTitleCase(name)
	g.needJSON = true
	hasDefaults := len(defaults) > 0
	hasTolerant := len(tolerant) > 0
	if hasDefaults {
		g.needFmt = true
	}

	fmt.Fprintf(&g.buf, "func (v *%s) UnmarshalJSON(data []byte) error {\n", goName)
	fmt.Fprintf(&g.buf, "\ttype Alias %s\n", goName)
	fmt.Fprintf(&g.buf, "\tvar a Alias\n")

	if hasDefaults {
		// Defaults need the raw key set on the conformant path too, so parse it
		// unconditionally and keep the strict error for non-object payloads.
		fmt.Fprintf(&g.buf, "\tstrictErr := json.Unmarshal(data, &a)\n")
		fmt.Fprintf(&g.buf, "\tvar raw map[string]json.RawMessage\n")
		fmt.Fprintf(&g.buf, "\tif err := json.Unmarshal(data, &raw); err != nil {\n")
		fmt.Fprintf(&g.buf, "\t\tif strictErr != nil {\n")
		fmt.Fprintf(&g.buf, "\t\t\treturn strictErr\n")
		fmt.Fprintf(&g.buf, "\t\t}\n")
		fmt.Fprintf(&g.buf, "\t\treturn fmt.Errorf(\"%s: decode raw fields: %%w\", err)\n", goName)
		fmt.Fprintf(&g.buf, "\t}\n")
		if hasTolerant {
			fmt.Fprintf(&g.buf, "\tif strictErr != nil {\n")
			g.writeTolerantRecoveryBody(tolerant, "\t\t")
			fmt.Fprintf(&g.buf, "\t}\n")
		} else {
			// No tolerant fields: a strict decode failure is a real error and
			// must be surfaced rather than masked by the default-application pass.
			fmt.Fprintf(&g.buf, "\tif strictErr != nil {\n")
			fmt.Fprintf(&g.buf, "\t\treturn strictErr\n")
			fmt.Fprintf(&g.buf, "\t}\n")
		}
	} else {
		// Tolerant-only: avoid the extra raw parse on the conformant path.
		fmt.Fprintf(&g.buf, "\tif err := json.Unmarshal(data, &a); err != nil {\n")
		fmt.Fprintf(&g.buf, "\t\tvar raw map[string]json.RawMessage\n")
		fmt.Fprintf(&g.buf, "\t\tif rawErr := json.Unmarshal(data, &raw); rawErr != nil {\n")
		fmt.Fprintf(&g.buf, "\t\t\treturn err\n")
		fmt.Fprintf(&g.buf, "\t\t}\n")
		g.writeTolerantRecoveryBody(tolerant, "\t\t")
		fmt.Fprintf(&g.buf, "\t}\n")
	}

	for _, d := range defaults {
		escaped := escapeGoString(d.rawJSON)
		fmt.Fprintf(&g.buf, "\tif rm, ok := raw[%q]; !ok || string(rm) == \"null\" {\n", d.jsonName)
		fmt.Fprintf(&g.buf, "\t\tif err := json.Unmarshal([]byte(%s), &a.%s); err != nil {\n", escaped, d.goName)
		fmt.Fprintf(&g.buf, "\t\t\treturn fmt.Errorf(\"%s: apply default for %s: %%w\", err)\n", goName, d.jsonName)
		fmt.Fprintf(&g.buf, "\t\t}\n")
		fmt.Fprintf(&g.buf, "\t}\n")
	}

	fmt.Fprintf(&g.buf, "\t*v = %s(a)\n", goName)
	fmt.Fprintf(&g.buf, "\treturn nil\n")
	fmt.Fprintf(&g.buf, "}\n\n")
}

// writeTolerantRecoveryBody emits the lenient recovery: sanitize the annotated
// fields in raw, re-marshal, and strict-decode again. Non-tolerant fields keep
// their strict semantics, so a re-decode failure surfaces as a real error.
func (g *Generator) writeTolerantRecoveryBody(tolerant []tolerantField, indent string) {
	for _, tf := range tolerant {
		if tf.skipInvalidItems {
			fmt.Fprintf(&g.buf, "%skeepDecodableItems[%s](raw, %q)\n", indent, tf.probeType, tf.jsonName)
		} else {
			fmt.Fprintf(&g.buf, "%sdropIfUndecodable[%s](raw, %q)\n", indent, tf.probeType, tf.jsonName)
		}
	}
	fmt.Fprintf(&g.buf, "%sfixed, mErr := json.Marshal(raw)\n", indent)
	fmt.Fprintf(&g.buf, "%sif mErr != nil {\n", indent)
	fmt.Fprintf(&g.buf, "%s\treturn mErr\n", indent)
	fmt.Fprintf(&g.buf, "%s}\n", indent)
	fmt.Fprintf(&g.buf, "%sa = Alias{}\n", indent)
	fmt.Fprintf(&g.buf, "%sif err := json.Unmarshal(fixed, &a); err != nil {\n", indent)
	fmt.Fprintf(&g.buf, "%s\treturn err\n", indent)
	fmt.Fprintf(&g.buf, "%s}\n", indent)
}

func (g *Generator) generateDropIfUndecodableHelper() {
	g.needJSON = true
	g.buf.WriteString("// dropIfUndecodable removes key from raw when its value is present, non-null,\n")
	g.buf.WriteString("// and fails to decode into T. It implements x-deserialize-default-on-error:\n")
	g.buf.WriteString("// a wrong-typed value is dropped so the field falls back to its default (an\n")
	g.buf.WriteString("// applied schema default, otherwise the Go zero value).\n")
	g.buf.WriteString("func dropIfUndecodable[T any](raw map[string]json.RawMessage, key string) {\n")
	g.buf.WriteString("\trm, ok := raw[key]\n")
	g.buf.WriteString("\tif !ok || string(rm) == \"null\" {\n")
	g.buf.WriteString("\t\treturn\n")
	g.buf.WriteString("\t}\n")
	g.buf.WriteString("\tvar probe T\n")
	g.buf.WriteString("\tif json.Unmarshal(rm, &probe) != nil {\n")
	g.buf.WriteString("\t\tdelete(raw, key)\n")
	g.buf.WriteString("\t}\n")
	g.buf.WriteString("}\n\n")
}

func (g *Generator) generateKeepDecodableItemsHelper() {
	g.needJSON = true
	g.buf.WriteString("// keepDecodableItems rewrites raw[key] to retain only the array elements that\n")
	g.buf.WriteString("// decode into T. It implements x-deserialize-skip-invalid-items. A present,\n")
	g.buf.WriteString("// non-null value that is not an array is dropped so the field falls back to its\n")
	g.buf.WriteString("// default (these fields are also x-deserialize-default-on-error).\n")
	g.buf.WriteString("func keepDecodableItems[T any](raw map[string]json.RawMessage, key string) {\n")
	g.buf.WriteString("\trm, ok := raw[key]\n")
	g.buf.WriteString("\tif !ok || string(rm) == \"null\" {\n")
	g.buf.WriteString("\t\treturn\n")
	g.buf.WriteString("\t}\n")
	g.buf.WriteString("\tvar items []json.RawMessage\n")
	g.buf.WriteString("\tif err := json.Unmarshal(rm, &items); err != nil {\n")
	g.buf.WriteString("\t\tdelete(raw, key)\n")
	g.buf.WriteString("\t\treturn\n")
	g.buf.WriteString("\t}\n")
	g.buf.WriteString("\tkept := make([]json.RawMessage, 0, len(items))\n")
	g.buf.WriteString("\tfor _, it := range items {\n")
	g.buf.WriteString("\t\tvar probe T\n")
	g.buf.WriteString("\t\tif json.Unmarshal(it, &probe) == nil {\n")
	g.buf.WriteString("\t\t\tkept = append(kept, it)\n")
	g.buf.WriteString("\t\t}\n")
	g.buf.WriteString("\t}\n")
	g.buf.WriteString("\tout, err := json.Marshal(kept)\n")
	g.buf.WriteString("\tif err != nil {\n")
	g.buf.WriteString("\t\tdelete(raw, key)\n")
	g.buf.WriteString("\t\treturn\n")
	g.buf.WriteString("\t}\n")
	g.buf.WriteString("\traw[key] = out\n")
	g.buf.WriteString("}\n\n")
}

// escapeGoString returns a Go string literal (with quotes) for a JSON value.
func escapeGoString(s string) string {
	return fmt.Sprintf("%q", s)
}
