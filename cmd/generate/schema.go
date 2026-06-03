package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// Schema represents a JSON Schema definition.
type Schema struct {
	Ref                  string             `json:"$ref,omitempty"`
	Defs                 map[string]*Schema `json:"$defs,omitempty"`
	Type                 SchemaType         `json:"type,omitempty"`
	Title                string             `json:"title,omitempty"`
	Description          *string            `json:"description,omitempty"`
	Properties           map[string]*Schema `json:"properties,omitempty"`
	Required             []string           `json:"required,omitempty"`
	Items                *Schema            `json:"items,omitempty"`
	Enum                 []json.RawMessage  `json:"enum,omitempty"`
	Const                *ConstValue        `json:"const,omitempty"`
	OneOf                []*Schema          `json:"oneOf,omitempty"`
	AnyOf                []*Schema          `json:"anyOf,omitempty"`
	AllOf                []*Schema          `json:"allOf,omitempty"`
	AdditionalProperties *AdditionalProps   `json:"additionalProperties,omitempty"`
	Discriminator        *Discriminator     `json:"discriminator,omitempty"`
	Default              json.RawMessage    `json:"default,omitempty"`
	XMethod_             string             `json:"x-method,omitempty"`
	XSide_               string             `json:"x-side,omitempty"`
}

// XMethod returns the x-method extension value.
func (s *Schema) XMethod() (string, bool) {
	return s.XMethod_, s.XMethod_ != ""
}

// XSide returns the x-side extension value.
func (s *Schema) XSide() (string, bool) {
	return s.XSide_, s.XSide_ != ""
}

// SchemaType handles JSON Schema type that can be a string or array of strings.
type SchemaType []string

func (st *SchemaType) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		*st = SchemaType{s}
		return nil
	}
	var arr []string
	if err := json.Unmarshal(data, &arr); err == nil {
		*st = SchemaType(arr)
		return nil
	}
	return fmt.Errorf("invalid schema type: %s", string(data))
}

func (st SchemaType) Contains(t string) bool {
	for _, v := range st {
		if v == t {
			return true
		}
	}
	return false
}

func (st SchemaType) IsNullable() bool {
	return st.Contains("null")
}

func (st SchemaType) NonNull() string {
	for _, v := range st {
		if v != "null" {
			return v
		}
	}
	return ""
}

// ConstValue wraps an arbitrary JSON constant.
type ConstValue struct {
	Value json.RawMessage
}

func (cv *ConstValue) UnmarshalJSON(data []byte) error {
	cv.Value = json.RawMessage(data)
	return nil
}

func (cv *ConstValue) StringValue() (string, bool) {
	if cv == nil {
		return "", false
	}
	var s string
	if err := json.Unmarshal(cv.Value, &s); err == nil {
		return s, true
	}
	return "", false
}

func (cv *ConstValue) IntValue() (int64, bool) {
	if cv == nil {
		return 0, false
	}
	var n int64
	if err := json.Unmarshal(cv.Value, &n); err == nil {
		return n, true
	}
	return 0, false
}

// AdditionalProps supports both boolean and schema forms.
type AdditionalProps struct {
	Bool   *bool
	Schema *Schema
}

func (ap *AdditionalProps) UnmarshalJSON(data []byte) error {
	var b bool
	if err := json.Unmarshal(data, &b); err == nil {
		ap.Bool = &b
		return nil
	}
	var s Schema
	if err := json.Unmarshal(data, &s); err == nil {
		ap.Schema = &s
		return nil
	}
	return fmt.Errorf("invalid additionalProperties: %s", string(data))
}

// Discriminator stores the propertyName for discriminated unions.
type Discriminator struct {
	PropertyName string `json:"propertyName"`
}

// Meta represents the meta.json method mapping.
type Meta struct {
	Version       int               `json:"version"`
	AgentMethods  map[string]string `json:"agentMethods"`
	ClientMethods map[string]string `json:"clientMethods"`
}

// GenerateType classifies a schema definition.
type GenerateType int

const (
	TypeUnknown GenerateType = iota
	TypeEnum
	TypeStruct
	TypeDiscriminatedUnion // oneOf/anyOf with discriminator
	TypePrimitive
	TypeArray
	TypeRef
	TypeSimpleUnion // simple union without discriminator
)

func (gt GenerateType) String() string {
	switch gt {
	case TypeEnum:
		return "Enum"
	case TypeStruct:
		return "Struct"
	case TypeDiscriminatedUnion:
		return "DiscriminatedUnion"
	case TypePrimitive:
		return "Primitive"
	case TypeArray:
		return "Array"
	case TypeRef:
		return "Ref"
	case TypeSimpleUnion:
		return "SimpleUnion"
	default:
		return "Unknown"
	}
}

// Definition holds a named schema definition and its classification.
type Definition struct {
	Name   string
	Schema *Schema
	Type   GenerateType
}

// LoadSchema loads and parses a JSON Schema file.
func LoadSchema(path string) (*Schema, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading schema: %w", err)
	}
	var s Schema
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parsing schema: %w", err)
	}
	return &s, nil
}

// LoadMeta loads and parses a meta.json file.
func LoadMeta(path string) (*Meta, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading meta: %w", err)
	}
	var m Meta
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parsing meta: %w", err)
	}
	return &m, nil
}

// GetDefinitions returns all definitions sorted by name, classified by type.
func GetDefinitions(s *Schema) []Definition {
	var defs []Definition
	for name, schema := range s.Defs {
		defs = append(defs, Definition{
			Name:   name,
			Schema: schema,
			Type:   classifyType(schema),
		})
	}
	sort.Slice(defs, func(i, j int) bool {
		return defs[i].Name < defs[j].Name
	})
	return defs
}

// classifyType determines the GenerateType for a schema.
func classifyType(s *Schema) GenerateType {
	// Has explicit enum field
	if len(s.Enum) > 0 {
		return TypeEnum
	}

	// Has $ref
	if s.Ref != "" {
		return TypeRef
	}

	// Handle oneOf
	if len(s.OneOf) > 0 {
		return classifyOneOf(s)
	}

	// Handle anyOf
	if len(s.AnyOf) > 0 {
		return classifyAnyOf(s)
	}

	// Handle allOf
	if len(s.AllOf) > 0 {
		if len(s.AllOf) == 1 && s.AllOf[0].Ref != "" {
			return TypeRef
		}
		return TypeStruct
	}

	// By type field
	switch {
	case len(s.Type) == 0:
		return TypeUnknown
	case s.Type.Contains("object"):
		return TypeStruct
	case s.Type.Contains("array"):
		return TypeArray
	case s.Type.Contains("string"):
		if len(s.Enum) > 0 {
			return TypeEnum
		}
		return TypePrimitive
	case s.Type.Contains("integer"), s.Type.Contains("number"), s.Type.Contains("boolean"):
		return TypePrimitive
	}

	return TypeUnknown
}

func classifyOneOf(s *Schema) GenerateType {
	variants := s.OneOf

	// All const values → enum
	allConst := true
	for _, v := range variants {
		if v.Const == nil {
			allConst = false
			break
		}
	}
	if allConst && len(variants) > 0 {
		return TypeEnum
	}

	// Has discriminator → discriminated union
	if s.Discriminator != nil {
		return TypeDiscriminatedUnion
	}

	// Detect discriminator from variants
	if disc := detectDiscriminator(variants); disc != "" {
		return TypeDiscriminatedUnion
	}

	// All refs → simple union
	allRefs := true
	for _, v := range filterNull(variants) {
		if v.Ref == "" && !(len(v.AllOf) == 1 && v.AllOf[0].Ref != "") {
			allRefs = false
			break
		}
	}
	if allRefs {
		return TypeSimpleUnion
	}

	return TypeStruct
}

func classifyAnyOf(s *Schema) GenerateType {
	variants := filterNull(s.AnyOf)

	if len(variants) == 0 {
		return TypeUnknown
	}

	if len(variants) == 1 {
		return classifyType(variants[0])
	}

	// All consts → enum
	constCount := 0
	for _, v := range variants {
		if v.Const != nil {
			constCount++
		}
	}
	if constCount >= len(variants)-1 && constCount >= 2 {
		return TypeEnum
	}

	// Has explicit discriminator → discriminated union
	if s.Discriminator != nil {
		return TypeDiscriminatedUnion
	}

	// Detect discriminator from variants (same as classifyOneOf)
	if disc := detectDiscriminator(variants); disc != "" {
		return TypeDiscriminatedUnion
	}

	// All refs (either direct $ref or allOf with single $ref) → simple union
	// Also handle array types whose items are refs
	allRefs := true
	for _, v := range variants {
		if v.Ref != "" {
			continue
		}
		if len(v.AllOf) == 1 && v.AllOf[0].Ref != "" {
			continue
		}
		if v.Type.Contains("array") && v.Items != nil && v.Items.Ref != "" {
			continue
		}
		allRefs = false
		break
	}
	if allRefs {
		return TypeSimpleUnion
	}

	return TypePrimitive
}

func filterNull(schemas []*Schema) []*Schema {
	var result []*Schema
	for _, s := range schemas {
		if s.Type.Contains("null") && len(s.Type) == 1 {
			continue
		}
		if s.Const != nil {
			if sv, ok := s.Const.StringValue(); ok && sv == "" {
				continue
			}
		}
		result = append(result, s)
	}
	return result
}

// detectDiscriminator finds a common const field across all variants.
func detectDiscriminator(variants []*Schema) string {
	if len(variants) < 2 {
		return ""
	}

	// Collect candidate fields: fields with const values
	candidates := make(map[string]int)
	for _, v := range variants {
		if v.Properties == nil {
			continue
		}
		for propName, propSchema := range v.Properties {
			if propSchema.Const != nil {
				candidates[propName]++
			}
		}
	}

	// A discriminator must appear in most variants
	for name, count := range candidates {
		if count >= len(variants)-1 { // allow one default variant
			return name
		}
	}
	return ""
}

// resolveRef extracts the type name from a $ref string.
func resolveRef(ref string) string {
	parts := strings.Split(ref, "/")
	return parts[len(parts)-1]
}

// unknownDiscriminatorFallback maps a definition name to its default variant Go
// name for unions that should fall back to the default variant when the wire
// discriminator is missing OR unknown (as opposed to only when missing). The
// value is the title-cased default variant short name and is cross-checked
// against the actual default variant at generation time. Adding an entry
// deliberately widens the union's decode semantics, so it stays an explicit,
// auditable allowlist rather than an inferred behavior.
var unknownDiscriminatorFallback = map[string]string{
	"SetSessionConfigOptionRequest": "ValueID",
}

// unionVariants returns the oneOf/anyOf variant list for a union schema,
// preferring oneOf.
func unionVariants(s *Schema) []*Schema {
	if len(s.OneOf) > 0 {
		return s.OneOf
	}
	return s.AnyOf
}

// parentSharedFieldNames returns the parent-level property names of a union
// schema, excluding the discriminator field. These are fields declared on the
// union object itself (alongside oneOf/anyOf) that every variant must carry.
func parentSharedFieldNames(s *Schema, discField string) []string {
	if s.Properties == nil {
		return nil
	}
	names := make([]string, 0, len(s.Properties))
	for name := range s.Properties {
		if name == discField {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// unionHasParentSharedFields reports whether a union carries parent-level shared
// fields (properties declared on the union object beyond the discriminator).
// Only unions with parent shared fields go through the extended generation path;
// unions without them keep their existing byte-for-byte output.
func unionHasParentSharedFields(s *Schema, discField string) bool {
	return len(parentSharedFieldNames(s, discField)) > 0
}

// resolveSingleRef returns the $ref target of a property schema, resolving a
// single-element allOf wrapper. Returns "" if the schema is not a plain ref.
func resolveSingleRef(s *Schema) string {
	if s == nil {
		return ""
	}
	if s.Ref != "" {
		return s.Ref
	}
	if len(s.AllOf) == 1 && s.AllOf[0].Ref != "" {
		return s.AllOf[0].Ref
	}
	return ""
}

// fieldSchemasEquivalent reports whether two property schemas have the same
// wire/validation semantics, ignoring descriptive metadata (title, description,
// x-*). A single-level $ref / allOf-ref is resolved against defs before
// comparison. Used to decide whether a parent field and a payload field with the
// same name may safely share a single struct field.
func fieldSchemasEquivalent(defs map[string]*Schema, a, b *Schema) bool {
	a = resolveForCompare(defs, a)
	b = resolveForCompare(defs, b)
	if a == nil || b == nil {
		return a == b
	}
	if !sameStringSet(a.Type, b.Type) {
		return false
	}
	if !sameRawJSON(constRaw(a.Const), constRaw(b.Const)) {
		return false
	}
	if !sameRawJSONSlice(a.Enum, b.Enum) {
		return false
	}
	if !sameRawJSON(a.Default, b.Default) {
		return false
	}
	if !sameStringSlice(a.Required, b.Required) {
		return false
	}
	if !sameUnionList(defs, a.OneOf, b.OneOf) {
		return false
	}
	if !sameUnionList(defs, a.AnyOf, b.AnyOf) {
		return false
	}
	if !sameUnionList(defs, a.AllOf, b.AllOf) {
		return false
	}
	if !fieldSchemasEquivalent(defs, a.Items, b.Items) {
		return false
	}
	if !samePropertyMap(defs, a.Properties, b.Properties) {
		return false
	}
	if !sameAdditionalProps(defs, a.AdditionalProperties, b.AdditionalProperties) {
		return false
	}
	return true
}

// resolveForCompare resolves a single-level $ref / allOf-ref against defs so two
// fields that reference the same alias compare equal.
func resolveForCompare(defs map[string]*Schema, s *Schema) *Schema {
	if s == nil {
		return nil
	}
	if ref := resolveSingleRef(s); ref != "" && defs != nil {
		if target, ok := defs[resolveRef(ref)]; ok {
			return target
		}
	}
	return s
}

func constRaw(c *ConstValue) json.RawMessage {
	if c == nil {
		return nil
	}
	return c.Value
}

func sameRawJSON(a, b json.RawMessage) bool {
	return string(a) == string(b)
}

func sameRawJSONSlice(a, b []json.RawMessage) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if string(a[i]) != string(b[i]) {
			return false
		}
	}
	return true
}

func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	am := make(map[string]int, len(a))
	for _, v := range a {
		am[v]++
	}
	for _, v := range b {
		am[v]--
		if am[v] < 0 {
			return false
		}
	}
	return true
}

func sameUnionList(defs map[string]*Schema, a, b []*Schema) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !fieldSchemasEquivalent(defs, a[i], b[i]) {
			return false
		}
	}
	return true
}

func samePropertyMap(defs map[string]*Schema, a, b map[string]*Schema) bool {
	if len(a) != len(b) {
		return false
	}
	for k, va := range a {
		vb, ok := b[k]
		if !ok || !fieldSchemasEquivalent(defs, va, vb) {
			return false
		}
	}
	return true
}

func sameAdditionalProps(defs map[string]*Schema, a, b *AdditionalProps) bool {
	if a == nil || b == nil {
		return a == b
	}
	if (a.Bool == nil) != (b.Bool == nil) {
		return false
	}
	if a.Bool != nil && *a.Bool != *b.Bool {
		return false
	}
	return fieldSchemasEquivalent(defs, a.Schema, b.Schema)
}
