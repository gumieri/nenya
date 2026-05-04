package config

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

func PrintSchema() (string, error) {
	schema := buildSchema(reflect.TypeOf(Config{}), map[reflect.Type]bool{})
	b, err := json.MarshalIndent(schema, "", "  ")
	if err != nil {
		return "", fmt.Errorf("failed to marshal schema: %w", err)
	}
	return string(b), nil
}

func buildSchema(t reflect.Type, seen map[reflect.Type]bool) map[string]any {
	s := map[string]any{}

	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}

	s["type"] = "object"
	if t.Name() != "" {
		s["title"] = t.Name()
	}

	var properties map[string]any
	requiredFields := []string{}

	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		if f.Anonymous {
			continue
		}

		tag := f.Tag.Get("json")
		name := f.Name
		if tag != "" {
			parts := strings.Split(tag, ",")
			if parts[0] == "-" {
				continue
			}
			if parts[0] != "" {
				name = parts[0]
			}
		}

		if properties == nil {
			properties = make(map[string]any)
		}

		fieldType := f.Type
		propSchema := buildFieldSchema(fieldType, seen)

		if desc := f.Tag.Get("description"); desc != "" {
			propSchema["description"] = desc
		}

		properties[name] = propSchema

		// Add to required if not omitempty
		if tag != "" && !strings.Contains(tag, "omitempty") {
			requiredFields = append(requiredFields, name)
		}
	}

	if properties != nil {
		s["properties"] = properties
	}
	if len(requiredFields) > 0 {
		s["required"] = requiredFields
	}

	return s
}

func buildFieldSchema(t reflect.Type, seen map[reflect.Type]bool) map[string]any {
	s := map[string]any{}

	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}

	switch t.Kind() {
	case reflect.String:
		s["type"] = "string"
	case reflect.Int, reflect.Int32, reflect.Int64:
		s["type"] = "integer"
	case reflect.Float32, reflect.Float64:
		s["type"] = "number"
	case reflect.Bool:
		s["type"] = "boolean"
	case reflect.Map:
		s["type"] = "object"
		s["additionalProperties"] = buildFieldSchema(t.Elem(), seen)
	case reflect.Slice, reflect.Array:
		s["type"] = "array"
		elemSchema := buildFieldSchema(t.Elem(), seen)
		if len(elemSchema) > 0 {
			s["items"] = elemSchema
		}
	case reflect.Struct:
		if t.Name() == "Duration" || t.Name() == "Time" {
			s["type"] = "string"
			return s
		}
		if seen[t] {
			s["$ref"] = "#/definitions/" + t.Name()
			return s
		}
		seen[t] = true
		schema := buildSchema(t, seen)
		for k, v := range schema {
			s[k] = v
		}
	default:
		s["type"] = "string"
	}

	return s
}
