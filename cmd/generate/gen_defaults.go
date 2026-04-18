package main

import (
	"encoding/json"
	"fmt"
)

// generateDefaultUnmarshalers generates UnmarshalJSON methods for struct types
// that have properties with "default" values in the schema.
func (g *Generator) generateDefaultUnmarshalers(defs []Definition) {
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
		if len(defaults) == 0 {
			continue
		}
		g.generateDefaultUnmarshalJSON(d.Name, defaults)
	}
}

type defaultEntry struct {
	jsonName string
	goName   string
	rawJSON  string // compact JSON string for the default value
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

func (g *Generator) generateDefaultUnmarshalJSON(name string, defaults []defaultEntry) {
	goName := toTitleCase(name)
	g.needJSON = true
	g.needFmt = true

	// Check if this type already has a custom UnmarshalJSON (from discriminated union generation).
	// Struct types with defaults should NOT conflict with union types.
	// Since discriminated unions are a separate GenerateType, this should be safe.

	fmt.Fprintf(&g.buf, "func (v *%s) UnmarshalJSON(data []byte) error {\n", goName)
	fmt.Fprintf(&g.buf, "\ttype Alias %s\n", goName)
	fmt.Fprintf(&g.buf, "\tvar a Alias\n")
	fmt.Fprintf(&g.buf, "\tif err := json.Unmarshal(data, &a); err != nil {\n")
	fmt.Fprintf(&g.buf, "\t\treturn err\n")
	fmt.Fprintf(&g.buf, "\t}\n")

	// Parse raw keys to detect missing/null fields. If the top-level unmarshal
	// into Alias succeeded the JSON must be an object, so a failure here is a
	// real anomaly worth surfacing rather than silently skipping defaults.
	fmt.Fprintf(&g.buf, "\tvar raw map[string]json.RawMessage\n")
	fmt.Fprintf(&g.buf, "\tif err := json.Unmarshal(data, &raw); err != nil {\n")
	fmt.Fprintf(&g.buf, "\t\treturn fmt.Errorf(\"%s: decode raw fields: %%w\", err)\n", goName)
	fmt.Fprintf(&g.buf, "\t}\n")

	for _, d := range defaults {
		// Escape the JSON for use in a Go string literal
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

// escapeGoString returns a Go string literal (with quotes) for a JSON value.
func escapeGoString(s string) string {
	return fmt.Sprintf("%q", s)
}
