package main

import (
	"strings"
	"unicode"
)

// goAcronyms maps lowercase abbreviations to their Go-conventional forms.
var goAcronyms = map[string]string{
	"id": "ID", "url": "URL", "uri": "URI",
	"http": "HTTP", "https": "HTTPS", "json": "JSON",
	"api": "API", "sql": "SQL", "ssh": "SSH",
	"tcp": "TCP", "udp": "UDP", "ip": "IP",
	"html": "HTML", "css": "CSS", "xml": "XML",
	"rpc": "RPC", "tls": "TLS", "ssl": "SSL",
	"eof": "EOF", "sse": "SSE", "mcp": "MCP",
	"fs": "FS", "ui": "UI", "io": "IO",
}

// toTitleCase converts snake_case, camelCase, or kebab-case to Go PascalCase
// with proper acronym handling.
func toTitleCase(s string) string {
	// Split into words by separators and case boundaries
	words := splitWords(s)
	var result strings.Builder
	for _, w := range words {
		lower := strings.ToLower(w)
		if acronym, ok := goAcronyms[lower]; ok {
			result.WriteString(acronym)
		} else {
			result.WriteString(capitalize(lower))
		}
	}
	return result.String()
}

// splitWords splits a string into words by underscores, hyphens, slashes,
// and camelCase boundaries.
func splitWords(s string) []string {
	var words []string
	var current strings.Builder

	flush := func() {
		if current.Len() > 0 {
			words = append(words, current.String())
			current.Reset()
		}
	}

	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case r == '_' || r == '-' || r == '/' || r == '.' || r == ' ':
			flush()
		case unicode.IsUpper(r):
			// Check if this starts a new word
			if current.Len() > 0 {
				// If previous was lowercase, start new word
				if i > 0 && unicode.IsLower(runes[i-1]) {
					flush()
				}
				// If next is lowercase and previous was upper, start new word
				// (handle "HTTPClient" -> "HTTP", "Client")
				if i > 0 && i+1 < len(runes) && unicode.IsUpper(runes[i-1]) && unicode.IsLower(runes[i+1]) {
					flush()
				}
			}
			current.WriteRune(r)
		default:
			current.WriteRune(r)
		}
	}
	flush()
	return words
}

// capitalize uppercases the first letter of a string.
func capitalize(s string) string {
	if s == "" {
		return s
	}
	runes := []rune(s)
	runes[0] = unicode.ToUpper(runes[0])
	return string(runes)
}

// uncapitalize lowercases the first letter of a string.
func uncapitalize(s string) string {
	if s == "" {
		return s
	}
	runes := []rune(s)
	runes[0] = unicode.ToLower(runes[0])
	return string(runes)
}

// receiverName returns a single-letter receiver variable for a Go type name.
func receiverName(typeName string) string {
	if typeName == "" {
		return "v"
	}
	return strings.ToLower(typeName[:1])
}

// mapJSONTypeToGo maps a JSON Schema type to a Go type string.
func mapJSONTypeToGo(jsonType string) string {
	switch jsonType {
	case "string":
		return "string"
	case "boolean":
		return "bool"
	case "number":
		return "float64"
	case "integer":
		return "int64"
	default:
		return jsonType
	}
}

// resolveGoType resolves a schema to a Go type string.
func resolveGoType(s *Schema, required bool) string {
	if s == nil {
		return "any"
	}

	if len(s.OneOf) > 0 {
		nonNull := filterNull(s.OneOf)
		if len(nonNull) == 1 {
			return resolveGoType(nonNull[0], required)
		}
		return "json.RawMessage"
	}

	if len(s.AnyOf) > 0 {
		nonNull := filterNull(s.AnyOf)
		if len(nonNull) == 1 {
			return resolveGoType(nonNull[0], required)
		}
		return "json.RawMessage"
	}

	// $ref
	if s.Ref != "" {
		typeName := toTitleCase(resolveRef(s.Ref))
		if !required {
			return "*" + typeName
		}
		return typeName
	}

	// allOf with single ref
	if len(s.AllOf) == 1 && s.AllOf[0].Ref != "" {
		typeName := toTitleCase(resolveRef(s.AllOf[0].Ref))
		if !required {
			return "*" + typeName
		}
		return typeName
	}

	// Handle nullable types: ["string", "null"]
	if len(s.Type) == 2 && s.Type.IsNullable() {
		baseType := mapJSONTypeToGo(s.Type.NonNull())
		if baseType == "string" {
			return "string" // strings use omitempty, not pointer
		}
		return "*" + baseType
	}

	if len(s.Type) == 0 {
		return "any"
	}

	baseType := s.Type[0]

	switch baseType {
	case "object":
		if s.AdditionalProperties != nil {
			if s.AdditionalProperties.Schema != nil {
				valType := resolveGoType(s.AdditionalProperties.Schema, true)
				return "map[string]" + valType
			}
			return "map[string]any"
		}
		if len(s.Properties) == 0 {
			return "map[string]any"
		}
		return "json.RawMessage" // preserve nested object unions without degrading to any
	case "array":
		if s.Items != nil {
			itemType := resolveGoType(s.Items, true)
			return "[]" + itemType
		}
		return "[]any"
	default:
		return mapJSONTypeToGo(baseType)
	}
}
