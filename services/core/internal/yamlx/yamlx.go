// Package yamlx converts YAML documents into JSON-compatible values, so the same
// structures can be validated with JSON Schema and decoded with encoding/json.
package yamlx

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"

	"go.yaml.in/yaml/v3"
)

// ToJSON parses YAML and returns the equivalent JSON document.
func ToJSON(data []byte) ([]byte, error) {
	var v any
	if err := yaml.Unmarshal(data, &v); err != nil {
		return nil, err
	}
	return json.Marshal(normalize(v))
}

// DecodeFile reads a YAML file and decodes it into out using JSON struct tags.
func DecodeFile(path string, out any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	js, err := ToJSON(data)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	dec := json.NewDecoder(bytes.NewReader(js))
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// ReadNative parses a YAML file into native Go values (int, float64, string, bool,
// map[string]any, []any). Integers stay integers, which CEL needs.
func ReadNative(path string, out any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := yaml.Unmarshal(data, out); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// normalize converts map[any]any (possible in YAML) into map[string]any.
func normalize(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			t[k] = normalize(val)
		}
		return t
	case map[any]any:
		m := make(map[string]any, len(t))
		for k, val := range t {
			m[fmt.Sprint(k)] = normalize(val)
		}
		return m
	case []any:
		for i := range t {
			t[i] = normalize(t[i])
		}
		return t
	default:
		return v
	}
}
