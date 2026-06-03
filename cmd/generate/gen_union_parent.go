package main

import (
	"fmt"
	"sort"
	"strings"
)

// objectUnionPlan is the resolved generation plan for an object union that
// carries parent-level shared fields. It captures everything the emit step
// needs so the templating logic stays declarative.
type objectUnionPlan struct {
	goName        string
	schema        *Schema
	discField     string // "" for non-discriminator anyOf unions
	isDiscrimin   bool   // true when a discriminator (explicit or derived) exists
	parentFields  []parentField
	parentReq     []string // parent required field json names (excludes discriminator)
	variants      []objectUnionVariant
	defaultIdx    int  // index of default variant, -1 if none
	unknownFallbk bool // allowlist: fall back to default on unknown discriminator
}

// parentField is a parent shared field carried by every variant wrapper.
type parentField struct {
	jsonName string
	goName   string
	goType   string
	required bool
	nullable bool
	// schema is the parent property schema, retained for conflict comparison.
	schema *Schema
}

// objectUnionVariant is a single resolved variant of an object union.
type objectUnionVariant struct {
	goTypeName string
	shortName  string
	constValue string // discriminator const, "" for default / non-discriminator
	isDefault  bool

	embedType string // ref payload type embedded in the wrapper, "" if none

	// embedReq are required field json names of the embedded ref payload, used
	// as additional structural distinguishing fields for non-discriminator anyOf.
	embedReq []string

	// payloadFields are the variant's own fields (inline object or
	// ref-in-property). Parent fields are merged in separately at emit time.
	payloadFields []variantField
	payloadReq    []string // payload required json names (excludes discriminator)

	// payloadFieldSchemas maps payload field json name to its source schema, for
	// parent/payload conflict comparison.
	payloadFieldSchemas map[string]*Schema

	// payloadIsUnion reports the embedded payload type is itself a union with a
	// custom MarshalJSON, so flattening must go through map-merge.
	payloadIsUnion bool
}

// variantField is a payload field of a variant wrapper.
type variantField struct {
	jsonName string
	goName   string
	goType   string
	required bool
	nullable bool
}

// mergedField is a final wrapper struct field after merging parent + payload.
type mergedField struct {
	jsonName  string
	goName    string
	goType    string
	required  bool
	nullable  bool
	fromParent bool
}

// generateObjectUnionWithParent emits the full SessionUpdate-style wrapper set
// for an object union that carries parent shared fields. It covers both
// discriminator unions and non-discriminator anyOf unions.
func (g *Generator) generateObjectUnionWithParent(plan *objectUnionPlan) {
	g.needJSON = true
	g.needFmt = true

	for i := range plan.variants {
		g.emitVariantWrapper(plan, &plan.variants[i])
	}

	g.emitUnionWrapper(plan)
	g.emitUnionMarshalJSON(plan)
	if plan.isDiscrimin {
		g.emitDiscriminatedUnmarshalJSON(plan)
	} else {
		g.emitStructuralUnmarshalJSON(plan)
	}
	g.emitUnionValidate(plan)
	g.emitUnionAccessors(plan)
	g.emitUnionConstructors(plan)
}

// mergedFields combines parent fields and payload fields for one variant,
// resolving same-name collisions and failing generation on incompatible ones.
func (g *Generator) mergedFields(plan *objectUnionPlan, v *objectUnionVariant) []mergedField {
	payloadByName := make(map[string]variantField, len(v.payloadFields))
	for _, pf := range v.payloadFields {
		payloadByName[pf.jsonName] = pf
	}

	var fields []mergedField
	parentNames := make(map[string]bool, len(plan.parentFields))
	for _, pf := range plan.parentFields {
		parentNames[pf.jsonName] = true
		if other, ok := payloadByName[pf.jsonName]; ok {
			if !g.parentPayloadCompatible(plan, v, pf, other) {
				g.fail("union %s variant %s: parent field %q conflicts with payload field of incompatible schema",
					plan.goName, v.goTypeName, pf.jsonName)
			}
			// Compatible: emit a single field carrying the parent definition.
		}
		fields = append(fields, mergedField{
			jsonName:   pf.jsonName,
			goName:     pf.goName,
			goType:     pf.goType,
			required:   pf.required,
			nullable:   pf.nullable,
			fromParent: true,
		})
	}
	for _, pf := range v.payloadFields {
		if parentNames[pf.jsonName] {
			continue
		}
		fields = append(fields, mergedField{
			jsonName: pf.jsonName,
			goName:   pf.goName,
			goType:   pf.goType,
			required: pf.required,
			nullable: pf.nullable,
		})
	}
	sort.SliceStable(fields, func(i, j int) bool { return fields[i].jsonName < fields[j].jsonName })
	return fields
}

func (g *Generator) parentPayloadCompatible(plan *objectUnionPlan, v *objectUnionVariant, pf parentField, vf variantField) bool {
	parentSchema := pf.schema
	var payloadSchema *Schema
	if plan.schema != nil {
		// payload field schema is looked up from the variant's own source schema
		payloadSchema = g.variantPayloadFieldSchema(v, vf.jsonName)
	}
	if parentSchema == nil || payloadSchema == nil {
		return false
	}
	if pf.required != vf.required {
		return false
	}
	return fieldSchemasEquivalent(g.defs(), parentSchema, payloadSchema)
}

func (g *Generator) defs() map[string]*Schema {
	if g.schema == nil {
		return nil
	}
	return g.schema.Defs
}

// emitVariantWrapper writes a single variant wrapper struct plus its
// MarshalJSON / UnmarshalJSON when the wrapper carries parent shared fields.
func (g *Generator) emitVariantWrapper(plan *objectUnionPlan, v *objectUnionVariant) {
	fields := g.mergedFields(plan, v)

	fmt.Fprintf(&g.buf, "type %s struct {\n", v.goTypeName)
	if v.embedType != "" {
		fmt.Fprintf(&g.buf, "\t%s\n", v.embedType)
	}
	for _, f := range fields {
		fmt.Fprintf(&g.buf, "\t%s %s `json:\"-\"`\n", f.goName, f.goType)
	}
	fmt.Fprintf(&g.buf, "}\n\n")

	g.emitVariantMarshalJSON(plan, v, fields)
	g.emitVariantUnmarshalJSON(plan, v, fields)
}

// emitVariantMarshalJSON flattens embedded payload + parent + payload fields +
// discriminator const into a single-level JSON object via map merge, so the
// final wire form never depends on encoding/json's anonymous-embed behavior.
func (g *Generator) emitVariantMarshalJSON(plan *objectUnionPlan, v *objectUnionVariant, fields []mergedField) {
	recv := receiverName(v.goTypeName)
	fmt.Fprintf(&g.buf, "func (%s %s) MarshalJSON() ([]byte, error) {\n", recv, v.goTypeName)
	fmt.Fprintf(&g.buf, "\tobj := map[string]json.RawMessage{}\n")
	if v.embedType != "" {
		fmt.Fprintf(&g.buf, "\t{\n")
		fmt.Fprintf(&g.buf, "\t\tpayload, err := json.Marshal(%s.%s)\n", recv, v.embedType)
		fmt.Fprintf(&g.buf, "\t\tif err != nil {\n")
		fmt.Fprintf(&g.buf, "\t\t\treturn nil, err\n")
		fmt.Fprintf(&g.buf, "\t\t}\n")
		fmt.Fprintf(&g.buf, "\t\tif len(payload) > 0 && string(payload) != \"null\" {\n")
		fmt.Fprintf(&g.buf, "\t\t\tif err := json.Unmarshal(payload, &obj); err != nil {\n")
		fmt.Fprintf(&g.buf, "\t\t\t\treturn nil, err\n")
		fmt.Fprintf(&g.buf, "\t\t\t}\n")
		fmt.Fprintf(&g.buf, "\t\t}\n")
		fmt.Fprintf(&g.buf, "\t}\n")
	}
	for _, f := range fields {
		g.emitFieldMarshal(recv, f)
	}
	if v.constValue != "" {
		fmt.Fprintf(&g.buf, "\tobj[%q], _ = json.Marshal(%q)\n", plan.discField, v.constValue)
	}
	fmt.Fprintf(&g.buf, "\treturn json.Marshal(obj)\n")
	fmt.Fprintf(&g.buf, "}\n\n")
}

// emitFieldMarshal writes one merged field into the obj map. Optional fields are
// skipped when zero (nil pointer / empty map / empty slice) to honor omitempty
// semantics; required fields are always written.
func (g *Generator) emitFieldMarshal(recv string, f mergedField) {
	access := recv + "." + f.goName
	isPtr := strings.HasPrefix(f.goType, "*")
	isSlice := strings.HasPrefix(f.goType, "[]")
	isMap := strings.HasPrefix(f.goType, "map[")
	emitWrite := func() {
		fmt.Fprintf(&g.buf, "\t\tb, err := json.Marshal(%s)\n", access)
		fmt.Fprintf(&g.buf, "\t\tif err != nil {\n")
		fmt.Fprintf(&g.buf, "\t\t\treturn nil, err\n")
		fmt.Fprintf(&g.buf, "\t\t}\n")
		fmt.Fprintf(&g.buf, "\t\tobj[%q] = b\n", f.jsonName)
	}
	switch {
	case !f.required && (isPtr || isSlice || isMap):
		// Optional reference-shaped field: omit when nil (omitempty semantics).
		fmt.Fprintf(&g.buf, "\tif %s != nil {\n", access)
		emitWrite()
		fmt.Fprintf(&g.buf, "\t}\n")
	case !f.required && f.goType == "string":
		// Optional string: omit when empty (matches struct ,omitempty output).
		fmt.Fprintf(&g.buf, "\tif %s != \"\" {\n", access)
		emitWrite()
		fmt.Fprintf(&g.buf, "\t}\n")
	case !f.required && f.goType == "bool":
		fmt.Fprintf(&g.buf, "\tif %s {\n", access)
		emitWrite()
		fmt.Fprintf(&g.buf, "\t}\n")
	default:
		fmt.Fprintf(&g.buf, "\t{\n")
		emitWrite()
		fmt.Fprintf(&g.buf, "\t}\n")
	}
}

// emitVariantUnmarshalJSON decodes the flat wire object into the embedded
// payload and the parent + payload fields. Presence is read from the raw key set
// so required scalars are not confused with legal zero values.
func (g *Generator) emitVariantUnmarshalJSON(plan *objectUnionPlan, v *objectUnionVariant, fields []mergedField) {
	recv := receiverName(v.goTypeName)
	fmt.Fprintf(&g.buf, "func (%s *%s) UnmarshalJSON(data []byte) error {\n", recv, v.goTypeName)
	fmt.Fprintf(&g.buf, "\t*%s = %s{}\n", recv, v.goTypeName)
	if v.embedType != "" {
		fmt.Fprintf(&g.buf, "\tif err := json.Unmarshal(data, &%s.%s); err != nil {\n", recv, v.embedType)
		fmt.Fprintf(&g.buf, "\t\treturn err\n")
		fmt.Fprintf(&g.buf, "\t}\n")
	}
	fmt.Fprintf(&g.buf, "\tvar raw map[string]json.RawMessage\n")
	fmt.Fprintf(&g.buf, "\tif err := json.Unmarshal(data, &raw); err != nil {\n")
	fmt.Fprintf(&g.buf, "\t\treturn err\n")
	fmt.Fprintf(&g.buf, "\t}\n")
	for _, f := range fields {
		fmt.Fprintf(&g.buf, "\tif rm, ok := raw[%q]; ok {\n", f.jsonName)
		fmt.Fprintf(&g.buf, "\t\tif err := json.Unmarshal(rm, &%s.%s); err != nil {\n", recv, f.goName)
		fmt.Fprintf(&g.buf, "\t\t\treturn err\n")
		fmt.Fprintf(&g.buf, "\t\t}\n")
		fmt.Fprintf(&g.buf, "\t}\n")
	}
	if checks := requiredPresenceChecks(plan, v, fields); len(checks) > 0 {
		for _, c := range checks {
			fmt.Fprintf(&g.buf, "\tif %s {\n", c.cond)
			fmt.Fprintf(&g.buf, "\t\treturn fmt.Errorf(%q)\n", c.msg)
			fmt.Fprintf(&g.buf, "\t}\n")
		}
	}
	fmt.Fprintf(&g.buf, "\treturn nil\n")
	fmt.Fprintf(&g.buf, "}\n\n")
}

type presenceCheck struct {
	cond string
	msg  string
}

// requiredPresenceChecks builds raw-key presence checks for required fields of a
// variant (parent required + payload required), excluding the discriminator.
// Required & non-nullable fields also reject an explicit JSON null.
func requiredPresenceChecks(plan *objectUnionPlan, v *objectUnionVariant, fields []mergedField) []presenceCheck {
	reqSet := make(map[string]bool)
	for _, r := range plan.parentReq {
		reqSet[r] = true
	}
	for _, r := range v.payloadReq {
		reqSet[r] = true
	}
	var checks []presenceCheck
	for _, f := range fields {
		if !reqSet[f.jsonName] {
			continue
		}
		if f.nullable {
			checks = append(checks, presenceCheck{
				cond: fmt.Sprintf("_, ok := raw[%q]; !ok", f.jsonName),
				msg:  fmt.Sprintf("%s is required", f.jsonName),
			})
			continue
		}
		checks = append(checks, presenceCheck{
			cond: fmt.Sprintf("rm, ok := raw[%q]; !ok || string(rm) == \"null\"", f.jsonName),
			msg:  fmt.Sprintf("%s is required", f.jsonName),
		})
	}
	return checks
}

func (g *Generator) emitUnionWrapper(plan *objectUnionPlan) {
	g.writeComment(plan.goName, plan.schema.Description)
	fmt.Fprintf(&g.buf, "type %s struct {\n", plan.goName)
	for _, v := range plan.variants {
		fmt.Fprintf(&g.buf, "\t%s *%s `json:\"-\"`\n", v.shortName, v.goTypeName)
	}
	fmt.Fprintf(&g.buf, "}\n\n")
}

// emitUnionMarshalJSON enforces exactly-one variant set, then delegates to the
// chosen variant's MarshalJSON.
func (g *Generator) emitUnionMarshalJSON(plan *objectUnionPlan) {
	recv := receiverName(plan.goName)
	fmt.Fprintf(&g.buf, "func (%s %s) MarshalJSON() ([]byte, error) {\n", recv, plan.goName)
	fmt.Fprintf(&g.buf, "\tset := 0\n")
	for _, v := range plan.variants {
		fmt.Fprintf(&g.buf, "\tif %s.%s != nil {\n\t\tset++\n\t}\n", recv, v.shortName)
	}
	fmt.Fprintf(&g.buf, "\tif set != 1 {\n")
	fmt.Fprintf(&g.buf, "\t\treturn nil, fmt.Errorf(\"%s: exactly one variant must be set, got %%d\", set)\n", plan.goName)
	fmt.Fprintf(&g.buf, "\t}\n")
	for _, v := range plan.variants {
		fmt.Fprintf(&g.buf, "\tif %s.%s != nil {\n", recv, v.shortName)
		fmt.Fprintf(&g.buf, "\t\treturn json.Marshal(%s.%s)\n", recv, v.shortName)
		fmt.Fprintf(&g.buf, "\t}\n")
	}
	fmt.Fprintf(&g.buf, "\treturn nil, fmt.Errorf(\"no variant is set for %s\")\n", plan.goName)
	fmt.Fprintf(&g.buf, "}\n\n")
}

func (g *Generator) emitDiscriminatedUnmarshalJSON(plan *objectUnionPlan) {
	recv := receiverName(plan.goName)
	discGo := toTitleCase(plan.discField)
	fmt.Fprintf(&g.buf, "func (%s *%s) UnmarshalJSON(data []byte) error {\n", recv, plan.goName)
	fmt.Fprintf(&g.buf, "\t*%s = %s{}\n", recv, plan.goName)
	fmt.Fprintf(&g.buf, "\tvar disc struct {\n")
	fmt.Fprintf(&g.buf, "\t\t%s *string `json:\"%s\"`\n", discGo, plan.discField)
	fmt.Fprintf(&g.buf, "\t}\n")
	fmt.Fprintf(&g.buf, "\tif err := json.Unmarshal(data, &disc); err != nil {\n")
	fmt.Fprintf(&g.buf, "\t\treturn err\n")
	fmt.Fprintf(&g.buf, "\t}\n")

	fmt.Fprintf(&g.buf, "\tif disc.%s != nil {\n", discGo)
	fmt.Fprintf(&g.buf, "\t\tswitch *disc.%s {\n", discGo)
	for i := range plan.variants {
		v := &plan.variants[i]
		if v.isDefault {
			continue
		}
		fmt.Fprintf(&g.buf, "\t\tcase %q:\n", v.constValue)
		fmt.Fprintf(&g.buf, "\t\t\tvar val %s\n", v.goTypeName)
		fmt.Fprintf(&g.buf, "\t\t\tif err := json.Unmarshal(data, &val); err != nil {\n")
		fmt.Fprintf(&g.buf, "\t\t\t\treturn err\n")
		fmt.Fprintf(&g.buf, "\t\t\t}\n")
		fmt.Fprintf(&g.buf, "\t\t\t%s.%s = &val\n", recv, v.shortName)
		fmt.Fprintf(&g.buf, "\t\t\treturn nil\n")
	}
	fmt.Fprintf(&g.buf, "\t\tdefault:\n")
	if plan.unknownFallbk && plan.defaultIdx >= 0 {
		dv := &plan.variants[plan.defaultIdx]
		fmt.Fprintf(&g.buf, "\t\t\tvar val %s\n", dv.goTypeName)
		fmt.Fprintf(&g.buf, "\t\t\tif err := json.Unmarshal(data, &val); err != nil {\n")
		fmt.Fprintf(&g.buf, "\t\t\t\treturn err\n")
		fmt.Fprintf(&g.buf, "\t\t\t}\n")
		fmt.Fprintf(&g.buf, "\t\t\t%s.%s = &val\n", recv, dv.shortName)
		fmt.Fprintf(&g.buf, "\t\t\treturn nil\n")
	} else {
		fmt.Fprintf(&g.buf, "\t\t\treturn fmt.Errorf(\"unknown discriminator value: %%s\", *disc.%s)\n", discGo)
	}
	fmt.Fprintf(&g.buf, "\t\t}\n")
	fmt.Fprintf(&g.buf, "\t}\n")

	// Discriminator absent: only the unique default variant may decode.
	if plan.defaultIdx >= 0 {
		dv := &plan.variants[plan.defaultIdx]
		fmt.Fprintf(&g.buf, "\tvar val %s\n", dv.goTypeName)
		fmt.Fprintf(&g.buf, "\tif err := json.Unmarshal(data, &val); err != nil {\n")
		fmt.Fprintf(&g.buf, "\t\treturn err\n")
		fmt.Fprintf(&g.buf, "\t}\n")
		fmt.Fprintf(&g.buf, "\t%s.%s = &val\n", recv, dv.shortName)
		fmt.Fprintf(&g.buf, "\treturn nil\n")
	} else {
		fmt.Fprintf(&g.buf, "\treturn fmt.Errorf(\"%s: missing discriminator %s\")\n", plan.goName, plan.discField)
	}
	fmt.Fprintf(&g.buf, "}\n\n")
}

// emitStructuralUnmarshalJSON decodes a non-discriminator anyOf union by
// required-field presence: exactly one variant whose required fields are all
// present is selected; zero matches and multiple matches are errors.
func (g *Generator) emitStructuralUnmarshalJSON(plan *objectUnionPlan) {
	g.needHasKey = true
	recv := receiverName(plan.goName)
	fmt.Fprintf(&g.buf, "func (%s *%s) UnmarshalJSON(data []byte) error {\n", recv, plan.goName)
	fmt.Fprintf(&g.buf, "\t*%s = %s{}\n", recv, plan.goName)
	fmt.Fprintf(&g.buf, "\tvar raw map[string]json.RawMessage\n")
	fmt.Fprintf(&g.buf, "\tif err := json.Unmarshal(data, &raw); err != nil {\n")
	fmt.Fprintf(&g.buf, "\t\treturn err\n")
	fmt.Fprintf(&g.buf, "\t}\n")
	fmt.Fprintf(&g.buf, "\tmatched := 0\n")

	for i := range plan.variants {
		v := &plan.variants[i]
		disc := variantDistinguishingFields(v)
		if len(disc) == 0 {
			g.fail("union %s variant %s: no required fields to distinguish non-discriminator anyOf variant",
				plan.goName, v.goTypeName)
			continue
		}
		conds := make([]string, 0, len(disc))
		for _, name := range disc {
			conds = append(conds, fmt.Sprintf("hasKey(raw, %q)", name))
		}
		fmt.Fprintf(&g.buf, "\tif %s {\n", strings.Join(conds, " && "))
		fmt.Fprintf(&g.buf, "\t\tvar val %s\n", v.goTypeName)
		fmt.Fprintf(&g.buf, "\t\tif err := json.Unmarshal(data, &val); err != nil {\n")
		fmt.Fprintf(&g.buf, "\t\t\treturn err\n")
		fmt.Fprintf(&g.buf, "\t\t}\n")
		fmt.Fprintf(&g.buf, "\t\t%s.%s = &val\n", recv, v.shortName)
		fmt.Fprintf(&g.buf, "\t\tmatched++\n")
		fmt.Fprintf(&g.buf, "\t}\n")
	}

	fmt.Fprintf(&g.buf, "\tif matched == 0 {\n")
	fmt.Fprintf(&g.buf, "\t\treturn fmt.Errorf(\"%s: data does not match any variant\")\n", plan.goName)
	fmt.Fprintf(&g.buf, "\t}\n")
	fmt.Fprintf(&g.buf, "\tif matched > 1 {\n")
	fmt.Fprintf(&g.buf, "\t\t*%s = %s{}\n", recv, plan.goName)
	fmt.Fprintf(&g.buf, "\t\treturn fmt.Errorf(\"%s: ambiguous union, data matches multiple variants\")\n", plan.goName)
	fmt.Fprintf(&g.buf, "\t}\n")
	fmt.Fprintf(&g.buf, "\treturn nil\n")
	fmt.Fprintf(&g.buf, "}\n\n")
}

// variantDistinguishingFields returns the required field json names of a variant
// used as the structural signature for non-discriminator anyOf decoding. Parent
// required fields are shared across variants, so only payload required fields
// (the variant's own + its embedded ref payload's) distinguish; if a variant has
// none, the caller fails generation.
func variantDistinguishingFields(v *objectUnionVariant) []string {
	set := make(map[string]bool)
	for _, r := range v.payloadReq {
		set[r] = true
	}
	for _, r := range v.embedReq {
		set[r] = true
	}
	out := make([]string, 0, len(set))
	for r := range set {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// emitUnionValidate enforces exactly-one variant and recurses into the selected
// variant's required checks + payload Validate(). It wires into the dispatch
// validatable interface at the request boundary.
func (g *Generator) emitUnionValidate(plan *objectUnionPlan) {
	recv := receiverName(plan.goName)
	fmt.Fprintf(&g.buf, "func (%s *%s) Validate() error {\n", recv, plan.goName)
	fmt.Fprintf(&g.buf, "\tset := 0\n")
	for _, v := range plan.variants {
		fmt.Fprintf(&g.buf, "\tif %s.%s != nil {\n\t\tset++\n\t}\n", recv, v.shortName)
	}
	fmt.Fprintf(&g.buf, "\tif set != 1 {\n")
	fmt.Fprintf(&g.buf, "\t\treturn fmt.Errorf(\"%s: exactly one variant must be set, got %%d\", set)\n", plan.goName)
	fmt.Fprintf(&g.buf, "\t}\n")
	for i := range plan.variants {
		v := &plan.variants[i]
		fmt.Fprintf(&g.buf, "\tif %s.%s != nil {\n", recv, v.shortName)
		fmt.Fprintf(&g.buf, "\t\treturn %s.%s.Validate()\n", recv, v.shortName)
		fmt.Fprintf(&g.buf, "\t}\n")
	}
	fmt.Fprintf(&g.buf, "\treturn nil\n")
	fmt.Fprintf(&g.buf, "}\n\n")

	for i := range plan.variants {
		g.emitVariantValidate(plan, &plan.variants[i])
	}
}

// emitVariantValidate checks parent + payload required fields (pointer-nil /
// empty for presence) and recurses into the embedded payload Validate().
func (g *Generator) emitVariantValidate(plan *objectUnionPlan, v *objectUnionVariant) {
	recv := receiverName(v.goTypeName)
	fields := g.mergedFields(plan, v)
	reqSet := make(map[string]bool)
	for _, r := range plan.parentReq {
		reqSet[r] = true
	}
	for _, r := range v.payloadReq {
		reqSet[r] = true
	}
	fmt.Fprintf(&g.buf, "func (%s *%s) Validate() error {\n", recv, v.goTypeName)
	for _, f := range fields {
		if !reqSet[f.jsonName] {
			continue
		}
		switch {
		case strings.HasPrefix(f.goType, "*"), strings.HasPrefix(f.goType, "[]"), strings.HasPrefix(f.goType, "map["):
			fmt.Fprintf(&g.buf, "\tif %s.%s == nil {\n", recv, f.goName)
			fmt.Fprintf(&g.buf, "\t\treturn fmt.Errorf(\"%s is required\")\n", f.jsonName)
			fmt.Fprintf(&g.buf, "\t}\n")
		case f.goType == "string":
			// presence-only: required strings allow explicit empty value
		}
	}
	if v.embedType != "" {
		fmt.Fprintf(&g.buf, "\tif validator, ok := any(&%s.%s).(interface{ Validate() error }); ok {\n", recv, v.embedType)
		fmt.Fprintf(&g.buf, "\t\tif err := validator.Validate(); err != nil {\n")
		fmt.Fprintf(&g.buf, "\t\t\treturn err\n")
		fmt.Fprintf(&g.buf, "\t\t}\n")
		fmt.Fprintf(&g.buf, "\t}\n")
	}
	fmt.Fprintf(&g.buf, "\treturn nil\n")
	fmt.Fprintf(&g.buf, "}\n\n")
}

func (g *Generator) emitUnionAccessors(plan *objectUnionPlan) {
	recv := receiverName(plan.goName)
	for _, v := range plan.variants {
		fmt.Fprintf(&g.buf, "func (%s *%s) As%s() (%s, bool) {\n", recv, plan.goName, v.shortName, v.goTypeName)
		fmt.Fprintf(&g.buf, "\tif %s.%s == nil {\n", recv, v.shortName)
		fmt.Fprintf(&g.buf, "\t\tvar zero %s\n", v.goTypeName)
		fmt.Fprintf(&g.buf, "\t\treturn zero, false\n")
		fmt.Fprintf(&g.buf, "\t}\n")
		fmt.Fprintf(&g.buf, "\treturn *%s.%s, true\n", recv, v.shortName)
		fmt.Fprintf(&g.buf, "}\n\n")
	}
}

// emitUnionConstructors generates New<Union><Variant> taking the full variant
// wrapper (parent fields + payload) and stamping the discriminator const.
func (g *Generator) emitUnionConstructors(plan *objectUnionPlan) {
	for i := range plan.variants {
		v := &plan.variants[i]
		funcName := "New" + plan.goName + v.shortName
		fmt.Fprintf(&g.buf, "// %s creates a %s holding a %s variant.\n", funcName, plan.goName, v.shortName)
		fmt.Fprintf(&g.buf, "func %s(v %s) %s {\n", funcName, v.goTypeName, plan.goName)
		fmt.Fprintf(&g.buf, "\treturn %s{%s: &v}\n", plan.goName, v.shortName)
		fmt.Fprintf(&g.buf, "}\n\n")
	}
}
