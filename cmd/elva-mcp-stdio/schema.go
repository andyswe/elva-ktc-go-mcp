package main

import (
	"reflect"

	"github.com/google/jsonschema-go/jsonschema"
)

// schemaFor builds the JSON Schema of a tool's input or output type in a portable
// form. The SDK's default inference spells nullable values (Go pointers and slices)
// as "type": ["null", "integer"]. That is valid JSON Schema, but clients that map
// tool schemas onto a single-type dialect (such as the OpenAPI subset some model
// providers use) reject or misread it. So every multi-type schema is rewritten to
// "anyOf": [{"type": "null"}, {"type": "integer", ...}], which means the same.
func schemaFor[T any]() (*jsonschema.Schema, error) {
	t := reflect.TypeFor[T]()
	if t.Kind() == reflect.Pointer {
		t = t.Elem() // a tool result is an object, even when returned by pointer
	}
	s, err := jsonschema.ForType(t, nil)
	if err != nil {
		return nil, err
	}
	portable(s)
	return s, nil
}

// portable rewrites s and all schemas nested in it so that none has more than one type.
func portable(s *jsonschema.Schema) {
	if s == nil {
		return
	}
	for _, c := range []*jsonschema.Schema{s.Items, s.AdditionalProperties, s.Contains, s.Not,
		s.If, s.Then, s.Else, s.PropertyNames, s.UnevaluatedItems, s.UnevaluatedProperties} {
		portable(c)
	}
	for _, list := range [][]*jsonschema.Schema{s.PrefixItems, s.AnyOf, s.AllOf, s.OneOf} {
		for _, c := range list {
			portable(c)
		}
	}
	for _, m := range []map[string]*jsonschema.Schema{s.Properties, s.PatternProperties, s.Defs, s.Definitions, s.DependentSchemas} {
		for _, c := range m {
			portable(c)
		}
	}
	if len(s.Types) < 2 {
		return
	}
	// One branch per type: the non-null branch keeps the type's own keywords
	// (properties, items, ...), the description stays on the outer schema.
	var branches []*jsonschema.Schema
	for _, typ := range s.Types {
		if typ == "null" {
			branches = append(branches, &jsonschema.Schema{Type: "null"})
			continue
		}
		b := *s
		b.Types, b.Type, b.Description = nil, typ, ""
		branches = append(branches, &b)
	}
	*s = jsonschema.Schema{Description: s.Description, AnyOf: branches}
}
