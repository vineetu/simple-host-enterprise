package mcp

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"
)

// A deliberately small JSON Schema checker, for tests: it understands exactly
// the keywords the output schemas use (type, including type arrays for
// nullable values; properties; required; items; additionalProperties; enum;
// description) and refuses any other keyword, so a schema cannot quietly rely
// on one that goes unchecked. It lives outside a _test file because both this
// package's tests and internal/handler's end-to-end tests use it; nothing in
// the server calls it, so the linker leaves it out of the binary.

var schemaKeywords = map[string]bool{
	"type": true, "properties": true, "required": true, "items": true,
	"additionalProperties": true, "enum": true, "description": true,
}

var schemaTypes = map[string]bool{
	"object": true, "array": true, "string": true, "integer": true,
	"number": true, "boolean": true, "null": true,
}

// normalizeJSON round-trips v through JSON, so Go-built schemas ([]string,
// map[string]any) and wire-decoded ones ([]any) look the same.
func normalizeJSON(v any) (any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out any
	err = json.Unmarshal(b, &out)
	return out, err
}

// CheckOutputSchema reports whether schema is a well-formed JSON Schema object
// of the kind MCP requires for Tool.outputSchema: a root of type "object",
// using only the supported keywords, each with a value of the right shape.
func CheckOutputSchema(schema any) error {
	norm, err := normalizeJSON(schema)
	if err != nil {
		return err
	}
	root, ok := norm.(map[string]any)
	if !ok {
		return fmt.Errorf("schema is %T, not an object", norm)
	}
	if root["type"] != "object" {
		return fmt.Errorf("root type is %v, want \"object\"", root["type"])
	}
	return checkSchemaNode(root, "$")
}

func checkSchemaNode(node map[string]any, at string) error {
	keys := make([]string, 0, len(node))
	for k := range node {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !schemaKeywords[k] {
			return fmt.Errorf("%s: unsupported keyword %q", at, k)
		}
	}
	if d, present := node["description"]; present {
		if _, ok := d.(string); !ok {
			return fmt.Errorf("%s: description must be a string", at)
		}
	}
	types, err := schemaTypeList(node, at)
	if err != nil {
		return err
	}
	has := func(t string) bool {
		for _, x := range types {
			if x == t {
				return true
			}
		}
		return len(types) == 0
	}

	var props map[string]any
	if raw, present := node["properties"]; present {
		if !has("object") {
			return fmt.Errorf("%s: properties on a non-object type", at)
		}
		var ok bool
		if props, ok = raw.(map[string]any); !ok {
			return fmt.Errorf("%s: properties must be an object", at)
		}
		names := make([]string, 0, len(props))
		for name := range props {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			sub, ok := props[name].(map[string]any)
			if !ok {
				return fmt.Errorf("%s.%s: property schema must be an object", at, name)
			}
			if err := checkSchemaNode(sub, at+"."+name); err != nil {
				return err
			}
		}
	}
	if raw, present := node["required"]; present {
		list, ok := raw.([]any)
		if !ok {
			return fmt.Errorf("%s: required must be an array", at)
		}
		seen := map[string]bool{}
		for _, r := range list {
			name, ok := r.(string)
			if !ok {
				return fmt.Errorf("%s: required entries must be strings", at)
			}
			if seen[name] {
				return fmt.Errorf("%s: %q required twice", at, name)
			}
			seen[name] = true
			if _, declared := props[name]; !declared {
				return fmt.Errorf("%s: %q is required but not a declared property", at, name)
			}
		}
	}
	if raw, present := node["additionalProperties"]; present {
		switch v := raw.(type) {
		case bool:
		case map[string]any:
			if err := checkSchemaNode(v, at+"[*]"); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%s: additionalProperties must be a boolean or a schema", at)
		}
	}
	if raw, present := node["items"]; present {
		if !has("array") {
			return fmt.Errorf("%s: items on a non-array type", at)
		}
		sub, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: items must be a schema", at)
		}
		if err := checkSchemaNode(sub, at+"[]"); err != nil {
			return err
		}
	} else if len(types) == 1 && types[0] == "array" {
		return fmt.Errorf("%s: an array needs an items schema", at)
	}
	if raw, present := node["enum"]; present {
		list, ok := raw.([]any)
		if !ok || len(list) == 0 {
			return fmt.Errorf("%s: enum must be a non-empty array", at)
		}
		typeOnly := map[string]any{}
		if t, present := node["type"]; present {
			typeOnly["type"] = t
		}
		for _, v := range list {
			if err := validateNode(typeOnly, v, at+"(enum)", nil); err != nil {
				return fmt.Errorf("%s: enum value %v does not match the type", at, v)
			}
		}
	}
	return nil
}

// schemaTypeList reads "type" as a list; empty means any type.
func schemaTypeList(node map[string]any, at string) ([]string, error) {
	raw, present := node["type"]
	if !present {
		return nil, nil
	}
	var list []any
	switch v := raw.(type) {
	case string:
		list = []any{v}
	case []any:
		if len(v) == 0 {
			return nil, fmt.Errorf("%s: type array is empty", at)
		}
		list = v
	default:
		return nil, fmt.Errorf("%s: type must be a string or an array of strings", at)
	}
	out := make([]string, 0, len(list))
	for _, t := range list {
		name, ok := t.(string)
		if !ok || !schemaTypes[name] {
			return nil, fmt.Errorf("%s: unknown type %v", at, t)
		}
		out = append(out, name)
	}
	return out, nil
}

// ValidateOutput checks value (any JSON-shaped Go value) against schema. When
// seen is not nil, it records the path of every declared property that was
// present in value, so a caller can confirm a schema declares nothing a tool
// never returns.
func ValidateOutput(schema, value any, seen map[string]bool) error {
	s, err := normalizeJSON(schema)
	if err != nil {
		return err
	}
	v, err := normalizeJSON(value)
	if err != nil {
		return err
	}
	node, ok := s.(map[string]any)
	if !ok {
		return fmt.Errorf("schema is %T, not an object", s)
	}
	return validateNode(node, v, "$", seen)
}

func validateNode(node map[string]any, value any, at string, seen map[string]bool) error {
	types, err := schemaTypeList(node, at)
	if err != nil {
		return err
	}
	if len(types) > 0 {
		matched := false
		for _, t := range types {
			if jsonTypeMatches(t, value) {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("%s: %s is not of type %s", at, describe(value), strings.Join(types, "|"))
		}
	}
	if raw, present := node["enum"]; present {
		ok := false
		for _, allowed := range raw.([]any) {
			if reflect.DeepEqual(allowed, value) {
				ok = true
				break
			}
		}
		if !ok {
			return fmt.Errorf("%s: %s is not one of %v", at, describe(value), raw)
		}
	}
	switch v := value.(type) {
	case map[string]any:
		props, _ := node["properties"].(map[string]any)
		if req, ok := node["required"].([]any); ok {
			for _, r := range req {
				if _, present := v[r.(string)]; !present {
					return fmt.Errorf("%s: required property %q is missing", at, r)
				}
			}
		}
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if sub, declared := props[k].(map[string]any); declared {
				if seen != nil {
					seen[at+"."+k] = true
				}
				if err := validateNode(sub, v[k], at+"."+k, seen); err != nil {
					return err
				}
				continue
			}
			switch extra := node["additionalProperties"].(type) {
			case bool:
				if !extra {
					return fmt.Errorf("%s: property %q is not allowed", at, k)
				}
			case map[string]any:
				if err := validateNode(extra, v[k], at+"."+k, seen); err != nil {
					return err
				}
			}
		}
	case []any:
		if items, ok := node["items"].(map[string]any); ok {
			for i, el := range v {
				// Items share one path, so coverage is per shape, not per index.
				if err := validateNode(items, el, at+"[]", seen); err != nil {
					return fmt.Errorf("%w (item %d)", err, i)
				}
			}
		}
	}
	return nil
}

func jsonTypeMatches(t string, value any) bool {
	switch t {
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "array":
		_, ok := value.([]any)
		return ok
	case "string":
		_, ok := value.(string)
		return ok
	case "number":
		_, ok := value.(float64)
		return ok
	case "integer":
		f, ok := value.(float64)
		return ok && f == math.Trunc(f) && !math.IsInf(f, 0)
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "null":
		return value == nil
	}
	return false
}

func describe(v any) string {
	b, _ := json.Marshal(v)
	s := string(b)
	if len(s) > 80 {
		s = s[:80] + "…"
	}
	return s
}

// SchemaPropertyPaths lists the path of every property a schema declares, in
// the form ValidateOutput records them.
func SchemaPropertyPaths(schema any) []string {
	norm, _ := normalizeJSON(schema)
	var out []string
	var walk func(node map[string]any, at string)
	walk = func(node map[string]any, at string) {
		if props, ok := node["properties"].(map[string]any); ok {
			for name, sub := range props {
				out = append(out, at+"."+name)
				if m, ok := sub.(map[string]any); ok {
					walk(m, at+"."+name)
				}
			}
		}
		if items, ok := node["items"].(map[string]any); ok {
			walk(items, at+"[]")
		}
	}
	if root, ok := norm.(map[string]any); ok {
		walk(root, "$")
	}
	sort.Strings(out)
	return out
}
