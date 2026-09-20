// Command schema-gen writes ../../config.schema.json (editor validation of
// config.yaml) and ../../chart/values.schema.json (enforced by Helm, with the
// config schema under `config`). Run it from this directory.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"reflect"

	jsonschema "github.com/swaggest/jsonschema-go"
)

const draft07 = "http://json-schema.org/draft-07/schema#" // what Helm documents

func main() {
	docs := newK8sDocs()

	outputs := []struct {
		path        string
		value       any
		title       string
		description string
	}{
		{"../../config.schema.json", config{}, "frigate-notify config", "Configuration file for frigate-notify"},
		{"../../chart/values.schema.json", values{}, "frigate-notify chart values", "Helm values for the frigate-notify chart"},
	}
	for _, out := range outputs {
		if err := write(out.path, out.value, out.title, out.description, docs); err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", out.path, err)
			os.Exit(1)
		}
		fmt.Printf("Wrote %s\n", out.path)
	}
}

func write(path string, v any, title, description string, docs *k8sDocs) error {
	schema, err := reflectSchema(v, docs)
	if err != nil {
		return fmt.Errorf("reflect: %w", err)
	}
	schema.WithSchema(draft07).WithTitle(title).WithDescription(description)

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(schema); err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

func reflectSchema(v any, docs *k8sDocs) (*jsonschema.Schema, error) {
	r := jsonschema.Reflector{}
	schema, err := r.Reflect(v,
		jsonschema.InlineRefs,
		jsonschema.InterceptSchema(func(p jsonschema.InterceptSchemaParams) (bool, error) {
			if !p.Value.IsValid() {
				return false, nil
			}
			if !p.Processed {
				return k8sQuantity(p.Value, p.Schema)
			}
			t := p.Value.Type()
			for t.Kind() == reflect.Pointer {
				t = t.Elem()
			}
			switch {
			case isK8sType(t):
				return false, docs.enrich(t, p.Schema)
			case t.Kind() == reflect.Struct && t.PkgPath() == "main":
				ownStruct(t, p.Schema)
			}
			return false, nil
		}),
	)
	return &schema, err
}

// ownStruct rejects unknown keys on our own structs, like the config loader.
// Kubernetes types stay open: a newer cluster's valid field must not be rejected.
func ownStruct(t reflect.Type, s *jsonschema.Schema) {
	closed := false
	s.AdditionalProperties = &jsonschema.SchemaOrBool{TypeBoolean: &closed}

	switch t {
	case reflect.TypeFor[unlessEntry]():
		s.MinProperties = 1
	case reflect.TypeFor[target]():
		s.AllOf = append(s.AllOf, targetRequirements()...)
	}
}

// targetRequirements makes each type require its backend's field, as
// Config.validateTarget does.
func targetRequirements() []jsonschema.SchemaOrBool {
	var allOf []jsonschema.SchemaOrBool
	for _, req := range []struct{ typ, field string }{
		{"hass", "service"},
		{"slack", "channel"},
		{"ntfy", "topic"},
		{"discord", "webhookURL"},
	} {
		isType := (&jsonschema.Schema{}).
			WithRequired("type").
			WithPropertiesItem("type", (&jsonschema.Schema{}).WithConst(req.typ).ToSchemaOrBool())

		field := &jsonschema.Schema{MinLength: 1}
		if req.typ == "discord" {
			field.WithPattern(`^https://[^/]+`)
		}
		then := (&jsonschema.Schema{}).
			WithRequired(req.field).
			WithPropertiesItem(req.field, field.ToSchemaOrBool())

		allOf = append(allOf, (&jsonschema.Schema{}).
			WithIf(isType.ToSchemaOrBool()).
			WithThen(then.ToSchemaOrBool()).
			ToSchemaOrBool())
	}
	return allOf
}
