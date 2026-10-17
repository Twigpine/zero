package mcp

import (
	"math"
	"strings"

	"github.com/Twigpine/zero/internal/tools"
)

func SchemaFromMCP(input map[string]any) tools.Schema {
	schema := tools.Schema{
		Type:                 firstString(input["type"], "object"),
		Properties:           map[string]tools.PropertySchema{},
		AdditionalProperties: boolValue(input["additionalProperties"], false),
	}

	schema.Required = append(schema.Required, stringSlice(input["required"])...)

	if properties, ok := input["properties"].(map[string]any); ok {
		for name, raw := range properties {
			propertyMap, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			schema.Properties[name] = propertyFromMCP(propertyMap)
		}
	}
	if len(schema.Properties) == 0 {
		schema.Properties = nil
	}
	return schema
}

func SchemaToMCP(schema tools.Schema) map[string]any {
	schemaType := strings.TrimSpace(schema.Type)
	if schemaType == "" {
		schemaType = "object"
	}
	output := map[string]any{
		"type":                 schemaType,
		"additionalProperties": schema.AdditionalProperties,
	}
	if len(schema.Required) > 0 {
		output["required"] = append([]string{}, schema.Required...)
	}
	if len(schema.Properties) > 0 {
		properties := make(map[string]any, len(schema.Properties))
		for name, property := range schema.Properties {
			properties[name] = propertyToMCP(property)
		}
		output["properties"] = properties
	}
	return output
}

func propertyToMCP(property tools.PropertySchema) map[string]any {
	propertyType := strings.TrimSpace(property.Type)
	if propertyType == "" {
		propertyType = "string"
	}
	output := map[string]any{"type": propertyType}
	if property.Description != "" {
		output["description"] = property.Description
	}
	if len(property.Enum) > 0 {
		output["enum"] = append([]string{}, property.Enum...)
	}
	if property.Default != nil {
		output["default"] = property.Default
	}
	if property.Minimum != nil {
		output["minimum"] = *property.Minimum
	}
	if property.Maximum != nil {
		output["maximum"] = *property.Maximum
	}
	if property.Items != nil {
		output["items"] = propertyToMCP(*property.Items)
	}
	if len(property.Properties) > 0 {
		nested := make(map[string]any, len(property.Properties))
		for name, child := range property.Properties {
			nested[name] = propertyToMCP(child)
		}
		output["properties"] = nested
	}
	if len(property.Required) > 0 {
		output["required"] = append([]string{}, property.Required...)
	}
	return output
}

func propertyFromMCP(input map[string]any) tools.PropertySchema {
	property := tools.PropertySchema{
		Type:        firstString(input["type"], "string"),
		Description: stringValue(input["description"]),
		Enum:        stringSlice(input["enum"]),
		Default:     input["default"],
	}
	if min, ok := intValue(input["minimum"]); ok {
		property.Minimum = &min
	}
	if max, ok := intValue(input["maximum"]); ok {
		property.Maximum = &max
	}
	if items, ok := input["items"].(map[string]any); ok {
		child := propertyFromMCP(items)
		property.Items = &child
	}
	if properties, ok := input["properties"].(map[string]any); ok {
		nested := make(map[string]tools.PropertySchema, len(properties))
		for name, raw := range properties {
			propertyMap, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			nested[name] = propertyFromMCP(propertyMap)
		}
		if len(nested) > 0 {
			property.Properties = nested
		}
	}
	property.Required = append(property.Required, stringSlice(input["required"])...)
	return property
}

func firstString(value any, fallback string) string {
	if text := stringValue(value); text != "" {
		return text
	}
	if values, ok := value.([]any); ok {
		for _, item := range values {
			if text := stringValue(item); text != "" {
				return text
			}
		}
	}
	return fallback
}

func stringValue(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	default:
		return ""
	}
}

func stringSlice(value any) []string {
	switch typed := value.(type) {
	case []string:
		return append([]string{}, typed...)
	case []any:
		values := make([]string, 0, len(typed))
		for _, item := range typed {
			if text := stringValue(item); text != "" {
				values = append(values, text)
			}
		}
		return values
	default:
		return nil
	}
}

func boolValue(value any, fallback bool) bool {
	if typed, ok := value.(bool); ok {
		return typed
	}
	return fallback
}

func intValue(value any) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case float64:
		if math.Trunc(typed) == typed {
			return int(typed), true
		}
	}
	return 0, false
}
