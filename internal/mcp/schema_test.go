package mcp

import (
	"reflect"
	"testing"

	"github.com/Twigpine/zero/internal/tools"
)

func intPtr(value int) *int {
	return &value
}

// A property describing an array of objects must round-trip its nested element
// shape (items + nested properties + required) through both MCP directions.
func TestPropertyToMCPMapsNestedArrayAndObjectShape(t *testing.T) {
	property := tools.PropertySchema{
		Type: "array",
		Items: &tools.PropertySchema{
			Type: "object",
			Properties: map[string]tools.PropertySchema{
				"path":   {Type: "string", Description: "file path"},
				"line":   {Type: "integer", Minimum: intPtr(1)},
				"nested": {Type: "object", Properties: map[string]tools.PropertySchema{"x": {Type: "string"}}, Required: []string{"x"}},
			},
			Required: []string{"path"},
		},
	}

	out := propertyToMCP(property)
	items, ok := out["items"].(map[string]any)
	if !ok {
		t.Fatalf("items missing or wrong type: %#v", out)
	}
	if items["type"] != "object" {
		t.Fatalf("items type = %#v, want object", items["type"])
	}
	props, ok := items["properties"].(map[string]any)
	if !ok {
		t.Fatalf("items.properties missing: %#v", items)
	}
	pathProp, ok := props["path"].(map[string]any)
	if !ok || pathProp["description"] != "file path" {
		t.Fatalf("items.properties.path = %#v", props["path"])
	}
	required, ok := items["required"].([]string)
	if !ok || len(required) != 1 || required[0] != "path" {
		t.Fatalf("items.required = %#v, want [path]", items["required"])
	}
	nested, ok := props["nested"].(map[string]any)
	if !ok {
		t.Fatalf("nested property missing: %#v", props)
	}
	if _, ok := nested["properties"].(map[string]any); !ok {
		t.Fatalf("nested.properties missing: %#v", nested)
	}
	if nestedReq, ok := nested["required"].([]string); !ok || len(nestedReq) != 1 || nestedReq[0] != "x" {
		t.Fatalf("nested.required = %#v, want [x]", nested["required"])
	}
}

// propertyFromMCP must reconstruct the same nested shape propertyToMCP emits.
func TestPropertyFromMCPMapsNestedArrayAndObjectShape(t *testing.T) {
	input := map[string]any{
		"type": "array",
		"items": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{"type": "string", "description": "file path"},
				"line": map[string]any{"type": "integer", "minimum": float64(1)},
			},
			"required": []any{"path"},
		},
	}

	property := propertyFromMCP(input)
	if property.Type != "array" {
		t.Fatalf("type = %q, want array", property.Type)
	}
	if property.Items == nil {
		t.Fatal("Items is nil, want reconstructed element schema")
	}
	if property.Items.Type != "object" {
		t.Fatalf("Items.Type = %q, want object", property.Items.Type)
	}
	if len(property.Items.Properties) != 2 {
		t.Fatalf("Items.Properties = %#v, want 2 entries", property.Items.Properties)
	}
	if property.Items.Properties["path"].Description != "file path" {
		t.Fatalf("Items.Properties.path = %#v", property.Items.Properties["path"])
	}
	if line := property.Items.Properties["line"]; line.Minimum == nil || *line.Minimum != 1 {
		t.Fatalf("Items.Properties.line.Minimum = %#v, want 1", line.Minimum)
	}
	if len(property.Items.Required) != 1 || property.Items.Required[0] != "path" {
		t.Fatalf("Items.Required = %#v, want [path]", property.Items.Required)
	}
}

// A full round-trip (tools -> MCP -> tools) must preserve nested object/array shape.
func TestSchemaRoundTripPreservesNestedShape(t *testing.T) {
	original := tools.PropertySchema{
		Type: "object",
		Properties: map[string]tools.PropertySchema{
			"edits": {
				Type: "array",
				Items: &tools.PropertySchema{
					Type:       "object",
					Properties: map[string]tools.PropertySchema{"old": {Type: "string"}, "new": {Type: "string"}},
					Required:   []string{"old", "new"},
				},
			},
		},
		Required: []string{"edits"},
	}

	round := propertyFromMCP(propertyToMCP(original))
	if !reflect.DeepEqual(round, original) {
		t.Fatalf("round-trip mismatch:\n got = %#v\nwant = %#v", round, original)
	}
}
