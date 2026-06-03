package main

import (
	"sort"
)

// buildObjectUnionPlan resolves a Definition into an objectUnionPlan for the
// extended (parent-shared-fields) generation path. It is only called for unions
// that carry parent shared fields. discField is "" for non-discriminator anyOf.
func (g *Generator) buildObjectUnionPlan(d Definition, discField string) *objectUnionPlan {
	s := d.Schema
	goName := toTitleCase(d.Name)
	variants := unionVariants(s)

	plan := &objectUnionPlan{
		goName:      goName,
		schema:      s,
		discField:   discField,
		isDiscrimin: discField != "",
		defaultIdx:  -1,
	}

	plan.parentFields, plan.parentReq = g.buildParentFields(s, discField)

	for idx, v := range variants {
		ov, ok := g.buildVariant(plan, idx, v)
		if !ok {
			return plan
		}
		plan.variants = append(plan.variants, ov)
	}

	g.resolveDefaultVariant(plan)
	g.validateAllowlist(d.Name, plan)
	g.checkNames(plan)

	return plan
}

// buildParentFields builds the parent shared fields (excluding the
// discriminator) and the parent required json-name list.
func (g *Generator) buildParentFields(s *Schema, discField string) ([]parentField, []string) {
	reqSet := make(map[string]bool)
	for _, r := range s.Required {
		reqSet[r] = true
	}
	var fields []parentField
	var required []string
	for _, name := range parentSharedFieldNames(s, discField) {
		prop := s.Properties[name]
		isReq := reqSet[name]
		pf := parentField{
			jsonName: name,
			goName:   toTitleCase(name),
			required: isReq,
			nullable: isNullableSchema(prop),
			schema:   prop,
		}
		if name == "_meta" {
			pf.goName = "Meta"
			pf.goType = "map[string]any"
		} else {
			pf.goType = g.unionFieldType(prop, isReq)
		}
		if isReq {
			required = append(required, name)
			g.failIfRequiredNullable(s, name, prop)
		}
		fields = append(fields, pf)
	}
	sort.Strings(required)
	return fields, required
}

// buildVariant resolves a single oneOf/anyOf variant into an objectUnionVariant.
// It covers: ref/allOf-ref payload with discriminator const; inline object;
// ref-in-property (ref nested inside a payload field's allOf).
func (g *Generator) buildVariant(plan *objectUnionPlan, idx int, v *Schema) (objectUnionVariant, bool) {
	ov := objectUnionVariant{}

	// Discriminator const, if this variant declares one.
	if plan.discField != "" && v.Properties != nil && v.Properties[plan.discField] != nil {
		if cv, ok := v.Properties[plan.discField].Const.StringValue(); ok {
			ov.constValue = cv
		}
	}

	// Top-level ref / allOf-ref payload to embed.
	embedRef := resolveSingleRef(v)
	if embedRef != "" {
		ov.embedType = toTitleCase(resolveRef(embedRef))
		ov.payloadIsUnion = g.refIsUnion(embedRef)
		ov.embedReq = g.refRequiredFields(embedRef)
	}

	// Payload fields declared directly on the variant (inline / ref-in-property),
	// excluding the discriminator field which is written by MarshalJSON.
	if v.Properties != nil {
		reqSet := make(map[string]bool)
		for _, r := range v.Required {
			reqSet[r] = true
		}
		ov.payloadFieldSchemas = make(map[string]*Schema)
		for _, name := range sortedKeys(v.Properties) {
			if name == plan.discField {
				continue
			}
			prop := v.Properties[name]
			ov.payloadFieldSchemas[name] = prop
			isReq := reqSet[name]
			vf := variantField{
				jsonName: name,
				goName:   toTitleCase(name),
				required: isReq,
				nullable: isNullableSchema(prop),
			}
			if name == "_meta" {
				vf.goName = "Meta"
				vf.goType = "map[string]any"
			} else {
				vf.goType = g.unionFieldType(prop, isReq)
			}
			ov.payloadFields = append(ov.payloadFields, vf)
			if isReq {
				ov.payloadReq = append(ov.payloadReq, name)
				g.failIfRequiredNullable(v, name, prop)
			}
		}
		sort.Strings(ov.payloadReq)
	}

	g.failIfArrayVariant(plan, v)

	// Naming: const-bearing variant uses const; otherwise this is a default /
	// structural variant named from title.
	if ov.constValue != "" {
		ov.shortName = toTitleCase(ov.constValue)
		ov.goTypeName = plan.goName + ov.shortName
		if ov.goTypeName == ov.embedType {
			ov.shortName += "Variant"
			ov.goTypeName = plan.goName + ov.shortName
		}
	} else {
		ov.isDefault = true
		name := g.structuralVariantName(plan, v)
		if name == "" {
			g.fail("union %s variant #%d: no const and no usable name (title/ref) for naming", plan.goName, idx)
			return ov, false
		}
		ov.shortName = name
		ov.goTypeName = plan.goName + name
	}

	return ov, true
}

// structuralVariantName derives the short name for a non-const variant. For a
// non-discriminator anyOf union whose variant is a ref / allOf-ref, the existing
// ref-derived naming style is preserved; otherwise the variant title is used.
func (g *Generator) structuralVariantName(plan *objectUnionPlan, v *Schema) string {
	if !plan.isDiscrimin {
		if ref := resolveSingleRef(v); ref != "" {
			return toTitleCase(resolveRef(ref))
		}
	}
	return variantTitleName(v)
}

// variantTitleName returns the title-cased title of a variant for naming
// non-const variants. Returns "" when title is missing / empty / normalizes to
// empty.
func variantTitleName(v *Schema) string {
	if v.Title == "" {
		return ""
	}
	return toTitleCase(v.Title)
}

// resolveDefaultVariant locates the unique default (no-const) variant of a
// discriminator union and records its index, failing on ambiguity.
func (g *Generator) resolveDefaultVariant(plan *objectUnionPlan) {
	if !plan.isDiscrimin {
		return
	}
	idx := -1
	for i := range plan.variants {
		if plan.variants[i].isDefault {
			if idx >= 0 {
				g.fail("union %s: multiple default (no-const) variants (#%d and #%d)", plan.goName, idx, i)
				return
			}
			idx = i
		}
	}
	plan.defaultIdx = idx
}

// validateAllowlist cross-checks the unknown-discriminator fallback allowlist
// against the resolved plan: the union must have a unique default variant whose
// short name matches the allowlisted value.
func (g *Generator) validateAllowlist(defName string, plan *objectUnionPlan) {
	want, ok := unknownDiscriminatorFallback[defName]
	if !ok {
		return
	}
	if plan.defaultIdx < 0 {
		g.fail("union %s: allowlisted for unknown-discriminator fallback but has no unique default variant", plan.goName)
		return
	}
	got := plan.variants[plan.defaultIdx].shortName
	if got != want {
		g.fail("union %s: allowlist default variant %q does not match resolved default %q", plan.goName, want, got)
		return
	}
	plan.unknownFallbk = true
}

// checkNames fails generation on duplicate variant short names.
func (g *Generator) checkNames(plan *objectUnionPlan) {
	seen := make(map[string]bool, len(plan.variants))
	for i := range plan.variants {
		name := plan.variants[i].shortName
		if seen[name] {
			g.fail("union %s: duplicate variant name %q", plan.goName, name)
			return
		}
		seen[name] = true
	}
}

// unionFieldType resolves a variant/parent property to its Go type, applying
// pointer-presence for required scalars so missing values are distinguishable
// from legal zero values.
func (g *Generator) unionFieldType(prop *Schema, required bool) string {
	base := g.resolveFieldType(prop, required)
	if !required {
		return base
	}
	// Required scalars that may legally be a zero value get pointer presence.
	switch base {
	case "string":
		return "*string"
	case "bool":
		return "*bool"
	case "int64":
		return "*int64"
	case "float64":
		return "*float64"
	}
	// Required scalar alias (e.g. SessionConfigValueId resolves to a defined
	// string/number alias): use pointer presence too.
	if g.isScalarAliasType(prop) {
		return "*" + base
	}
	return base
}

// isScalarAliasType reports whether a property is a single $ref / allOf-ref to a
// primitive (scalar) alias definition.
func (g *Generator) isScalarAliasType(prop *Schema) bool {
	ref := resolveSingleRef(prop)
	if ref == "" {
		return false
	}
	defs := g.defs()
	if defs == nil {
		return false
	}
	target, ok := defs[resolveRef(ref)]
	if !ok {
		return false
	}
	switch classifyType(target) {
	case TypePrimitive:
		return true
	}
	return false
}

// refRequiredFields returns the required field json names of a $ref target,
// resolving a merged allOf if present.
func (g *Generator) refRequiredFields(ref string) []string {
	defs := g.defs()
	if defs == nil {
		return nil
	}
	target, ok := defs[resolveRef(ref)]
	if !ok {
		return nil
	}
	if len(target.AllOf) > 0 {
		target = mergeAllOf(target)
	}
	out := append([]string(nil), target.Required...)
	sort.Strings(out)
	return out
}

// refIsUnion reports whether a $ref target classifies as a union with a custom
// MarshalJSON (discriminated or simple).
func (g *Generator) refIsUnion(ref string) bool {
	defs := g.defs()
	if defs == nil {
		return false
	}
	target, ok := defs[resolveRef(ref)]
	if !ok {
		return false
	}
	switch classifyType(target) {
	case TypeDiscriminatedUnion, TypeSimpleUnion:
		return true
	}
	return false
}

// variantPayloadFieldSchema returns the source schema of a payload field by json
// name, used for parent/payload conflict comparison.
func (g *Generator) variantPayloadFieldSchema(v *objectUnionVariant, jsonName string) *Schema {
	if v.payloadFieldSchemas == nil {
		return nil
	}
	return v.payloadFieldSchemas[jsonName]
}

// failIfRequiredNullable fails generation if a required field permits JSON null,
// since presence checks cannot then distinguish "missing" from "explicit null"
// for manual construction.
func (g *Generator) failIfRequiredNullable(_ *Schema, name string, prop *Schema) {
	if isNullableSchema(prop) {
		g.fail("required field %q permits null; required nullable fields are unsupported in parent-shared unions", name)
	}
}

// failIfArrayVariant fails generation when a variant is an array type, since
// array variants combined with parent shared fields are unsupported.
func (g *Generator) failIfArrayVariant(plan *objectUnionPlan, v *Schema) {
	if v.Type.Contains("array") {
		g.fail("union %s: array variant combined with parent shared fields is unsupported", plan.goName)
	}
	if v.Items != nil {
		g.fail("union %s: array-of-ref variant combined with parent shared fields is unsupported", plan.goName)
	}
}

// isNullableSchema reports whether a schema permits a JSON null value, via a
// nullable type list or an anyOf null branch.
func isNullableSchema(s *Schema) bool {
	if s == nil {
		return false
	}
	if s.Type.IsNullable() {
		return true
	}
	for _, v := range s.AnyOf {
		if v != nil && v.Type.Contains("null") && len(v.Type) == 1 {
			return true
		}
	}
	for _, v := range s.OneOf {
		if v != nil && v.Type.Contains("null") && len(v.Type) == 1 {
			return true
		}
	}
	return false
}
