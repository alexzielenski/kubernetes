package main

import (
	"path/filepath"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/kube-openapi/pkg/validation/spec"
)

// isNonNullalbeNull returns true if the item is nil AND it's nullable
func isNonNullableNull(x interface{}, s *spec.Schema, defs map[string]*spec.Schema) bool {
	// Not possible to tell with spec.Schema if nullable is unset or not
	// (which would mean it is affirmatively overridden)
	resolved := resolveProperty(s, defs)
	return x == nil && s != nil && s.Nullable == false && resolved.Nullable == false
}

func defaultValue(sch *spec.Schema, defs map[string]*spec.Schema) interface{} {
	if sch == nil {
		return nil
	}

	if sch.Default != nil {
		return sch.Default
	}
	resolved := resolveProperty(sch, defs)
	return resolved.Default
}

func refOfSchema(sch *spec.Schema) string {
	if res := sch.Ref.String(); len(res) > 0 {
		return res
	}

	// For OpenAPI V3 some refs were stuffed inside allOf.
	// These refs are guaranteed to be the only element inside an allOf attached
	// to property due to construction of OpenAPI schema on server
	if len(sch.AllOf) == 1 && len(sch.AllOf[0].Ref.String()) > 0 {
		return sch.AllOf[0].Ref.String()
	}

	return ""
}

func resolveProperty(sch *spec.Schema, defs map[string]*spec.Schema) *spec.Schema {
	if sch == nil {
		return nil
	}

	if ref := refOfSchema(sch); len(ref) == 0 {
		return sch
	} else {
		lastPart := filepath.Base(ref)
		if existing, exists := defs[lastPart]; exists {
			return existing
		} else {
			// error?
		}
	}

	return sch
}

// Default does defaulting of x depending on default values in s.
// Default values from s are deep-copied.
//
// PruneNonNullableNullsWithoutDefaults has left the non-nullable nulls
// that have a default here.
//
// Code copied from structural schema defaulting, adapted for use with OpenAPI
// (not all native schemas are structural. e.g. CRDs are recursive)
func Default(x interface{}, s *spec.Schema, definitions map[string]*spec.Schema) {
	if s == nil {
		return
	}

	switch x := x.(type) {
	case map[string]interface{}:
		for k, prop := range s.Properties {
			def := defaultValue(&prop, definitions)
			if def == nil {
				continue
			}

			if _, found := x[k]; !found || isNonNullableNull(x[k], &prop, definitions) {
				x[k] = runtime.DeepCopyJSONValue(def)
			}
		}
		for k := range x {
			if prop, found := s.Properties[k]; found {
				resolved := resolveProperty(&prop, definitions)
				Default(x[k], resolved, definitions)
			} else if s.AdditionalProperties != nil {
				if isNonNullableNull(x[k], s.AdditionalProperties.Schema, definitions) {
					def := defaultValue(s.AdditionalItems.Schema, definitions)
					if def != nil {
						x[k] = runtime.DeepCopyJSONValue(def)
					}
				}
				Default(x[k], resolveProperty(s.AdditionalProperties.Schema, definitions), definitions)
			}
		}
	case []interface{}:
		def := defaultValue(s.Items.Schema, definitions)

		for i := range x {
			if isNonNullableNull(x[i], s.Items.Schema, definitions) {
				if def != nil {
					x[i] = runtime.DeepCopyJSONValue(def)
				}
			}
			Default(x[i], resolveProperty(s.Items.Schema, definitions), definitions)
		}
	default:
		// scalars, do nothing
	}
}
