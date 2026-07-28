// -------------------------------------------------------------------------------
// OpenAPI - Go Type to JSON Schema Reflection
//
// Author: Alex Freidah
//
// Derives JSON Schema from the admin wire types by reflection, so the schema
// cannot drift from the struct it describes: deleting a field deletes it from
// the description. Named struct types become entries under components/schemas
// and are referenced, so a type shared by several endpoints is described once.
// Handles the shapes the adminapi package actually uses - structs with json
// tags, embedded structs, pointers, slices, maps, time.Time and the basic
// kinds - and reports anything else rather than emitting a silent empty schema.
// -------------------------------------------------------------------------------

package openapi

import (
	"fmt"
	"maps"
	"reflect"
	"strings"
	"time"
)

// schemaSet collects the named schemas discovered during reflection, keyed by
// Go type name as it appears under components/schemas.
type schemaSet map[string]map[string]any

// timeType is special-cased to a date-time string rather than reflected as the
// struct it is.
var timeType = reflect.TypeFor[time.Time]()

// schemaFor returns the schema for v. Named struct types are registered in set
// and returned as a reference, so the document describes each type once.
func schemaFor(v any, set schemaSet) (map[string]any, error) {
	if v == nil {
		return map[string]any{}, nil
	}
	return schemaForType(reflect.TypeOf(v), set)
}

// schemaForType is schemaFor over a reflect.Type, recursing through pointers,
// slices and maps until it reaches something describable.
func schemaForType(t reflect.Type, set schemaSet) (map[string]any, error) {
	if t == timeType {
		return map[string]any{keyType: TypeString, keyFormat: "date-time"}, nil
	}

	switch t.Kind() {
	case reflect.Pointer:
		// A pointer only affects whether a field is required, which the
		// caller decides; the schema is the element's.
		return schemaForType(t.Elem(), set)

	case reflect.Struct:
		return structSchema(t, set)

	case reflect.Slice, reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 {
			return map[string]any{keyType: TypeString, keyFormat: "byte"}, nil
		}
		items, err := schemaForType(t.Elem(), set)
		if err != nil {
			return nil, err
		}
		return map[string]any{keyType: typeArray, "items": items}, nil

	case reflect.Map:
		if t.Key().Kind() != reflect.String {
			return nil, fmt.Errorf("map key must be a string, got %s", t.Key())
		}
		values, err := schemaForType(t.Elem(), set)
		if err != nil {
			return nil, err
		}
		return map[string]any{keyType: typeObject, "additionalProperties": values}, nil

	case reflect.String:
		return map[string]any{keyType: TypeString}, nil

	case reflect.Bool:
		return map[string]any{keyType: TypeBoolean}, nil

	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{keyType: TypeInteger, keyFormat: intFormat(t)}, nil

	case reflect.Float32, reflect.Float64:
		return map[string]any{keyType: typeNumber}, nil

	case reflect.Interface:
		// An empty schema is the honest description of an untyped value.
		return map[string]any{}, nil

	default:
		return nil, fmt.Errorf("cannot describe %s (kind %s)", t, t.Kind())
	}
}

// intFormat maps Go integer widths onto the two formats OpenAPI defines.
func intFormat(t reflect.Type) string {
	switch t.Kind() {
	case reflect.Int64, reflect.Uint64, reflect.Int, reflect.Uint:
		return "int64"
	default:
		return "int32"
	}
}

// structSchema registers t under components/schemas and returns a reference to
// it. Anonymous structs have no name to register, so they are inlined.
func structSchema(t reflect.Type, set schemaSet) (map[string]any, error) {
	if t.Name() == "" {
		return objectSchema(t, set)
	}
	ref := map[string]any{"$ref": "#/components/schemas/" + t.Name()}
	if _, done := set[t.Name()]; done {
		return ref, nil
	}
	// Reserve the name before recursing so a self-referential type
	// terminates instead of looping.
	set[t.Name()] = map[string]any{}
	obj, err := objectSchema(t, set)
	if err != nil {
		return nil, err
	}
	set[t.Name()] = obj
	return ref, nil
}

// objectSchema builds the property map for a struct, flattening embedded
// structs the way encoding/json does so the description matches the bytes on
// the wire rather than the Go type layout.
func objectSchema(t reflect.Type, set schemaSet) (map[string]any, error) {
	props := map[string]any{}
	var required []string

	for field := range t.Fields() {
		if err := addField(&field, t, props, &required, set); err != nil {
			return nil, err
		}
	}

	obj := map[string]any{keyType: typeObject, "properties": props}
	if len(required) > 0 {
		obj["required"] = required
	}
	return obj, nil
}

// addField folds one struct field into the property map, recursing into
// embedded structs so their fields land on the parent the way encoding/json
// writes them.
func addField(field *reflect.StructField, owner reflect.Type, props map[string]any, required *[]string, set schemaSet) error {
	if !field.IsExported() {
		return nil
	}
	name, opts := parseJSONTag(field)
	if name == "-" {
		return nil
	}

	if field.Anonymous && name == "" && derefKind(field.Type) == reflect.Struct && field.Type != timeType {
		embedded, err := objectSchema(deref(field.Type), set)
		if err != nil {
			return err
		}
		mergeObject(props, required, embedded)
		return nil
	}
	if name == "" {
		name = field.Name
	}

	schema, err := schemaForType(field.Type, set)
	if err != nil {
		return fmt.Errorf("field %s.%s: %w", owner.Name(), field.Name, err)
	}
	props[name] = schema

	// A field is required when it always appears: not omitempty, and not a
	// pointer whose nil is the way the handler omits it.
	if !opts.omitempty && field.Type.Kind() != reflect.Pointer {
		*required = append(*required, name)
	}
	return nil
}

// mergeObject folds an embedded struct's properties and required list into the
// parent's.
func mergeObject(props map[string]any, required *[]string, embedded map[string]any) {
	if p, ok := embedded["properties"].(map[string]any); ok {
		maps.Copy(props, p)
	}
	if r, ok := embedded["required"].([]string); ok {
		*required = append(*required, r...)
	}
}

// jsonOpts is the subset of encoding/json tag options that changes the schema.
type jsonOpts struct{ omitempty bool }

// parseJSONTag splits a field's json tag into its name and the options that
// matter here.
func parseJSONTag(f *reflect.StructField) (string, jsonOpts) {
	tag, ok := f.Tag.Lookup("json")
	if !ok {
		if f.Anonymous {
			return "", jsonOpts{}
		}
		return f.Name, jsonOpts{}
	}
	parts := strings.Split(tag, ",")
	var opts jsonOpts
	for _, p := range parts[1:] {
		if p == "omitempty" {
			opts.omitempty = true
		}
	}
	return parts[0], opts
}

// deref returns the element type of a pointer, or t unchanged.
func deref(t reflect.Type) reflect.Type {
	if t.Kind() == reflect.Pointer {
		return t.Elem()
	}
	return t
}

// derefKind is the kind of t with any pointer removed.
func derefKind(t reflect.Type) reflect.Kind { return deref(t).Kind() }
