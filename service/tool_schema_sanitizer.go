package service

import "github.com/QuantumNous/new-api/dto"

const maxToolSchemaSanitizeDepth = 64

func sanitizeOpenAIFunctionParameters(schema any) any {
	cleaned := sanitizeToolSchemaValue(schema, 0)
	if schemaMap, ok := cleaned.(map[string]any); ok {
		if _, exists := schemaMap["type"]; !exists {
			schemaMap["type"] = "object"
		}
		if _, exists := schemaMap["properties"]; !exists {
			schemaMap["properties"] = map[string]any{}
		}
		return schemaMap
	}

	return map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}
}

func SanitizeOpenAIFunctionToolParameters(request *dto.GeneralOpenAIRequest) {
	if request == nil {
		return
	}
	for i := range request.Tools {
		request.Tools[i].Function.Parameters = sanitizeOpenAIFunctionParameters(request.Tools[i].Function.Parameters)
	}
}

func sanitizeToolSchemaValue(value any, depth int) any {
	if depth >= maxToolSchemaSanitizeDepth {
		return sanitizeToolSchemaValueShallow(value)
	}

	switch typed := value.(type) {
	case map[string]any:
		return sanitizeToolSchemaMap(typed, depth)
	case []any:
		cleaned := make([]any, 0, len(typed))
		for _, item := range typed {
			cleaned = append(cleaned, sanitizeToolSchemaValue(item, depth+1))
		}
		return cleaned
	default:
		return typed
	}
}

func sanitizeToolSchemaValueShallow(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		cleaned := make(map[string]any, len(typed))
		for key, item := range typed {
			if item != nil {
				cleaned[key] = item
			}
		}
		return cleaned
	case []any:
		return append([]any(nil), typed...)
	default:
		return typed
	}
}

func sanitizeToolSchemaMap(schema map[string]any, depth int) map[string]any {
	cleaned := make(map[string]any, len(schema))
	for key, value := range schema {
		if value == nil {
			continue
		}

		switch key {
		case "type":
			normalizeSchemaType(cleaned, value)
		case "required":
			if required, ok := normalizeSchemaRequired(value); ok {
				cleaned[key] = required
			}
		case "properties":
			cleaned[key] = sanitizeSchemaProperties(value, depth+1)
		case "additionalProperties":
			if additionalProperties, ok := normalizeAdditionalProperties(value, depth+1); ok {
				cleaned[key] = additionalProperties
			}
		case "items":
			cleaned[key] = normalizeSchemaObject(value, depth+1)
		case "anyOf", "oneOf", "allOf":
			if items, ok := normalizeSchemaObjectList(value, depth+1); ok {
				cleaned[key] = items
			}
		default:
			cleaned[key] = sanitizeToolSchemaValue(value, depth+1)
		}
	}

	if _, hasType := cleaned["type"]; !hasType {
		if _, hasProperties := cleaned["properties"]; hasProperties {
			cleaned["type"] = "object"
		}
	}

	return cleaned
}

func normalizeSchemaType(target map[string]any, value any) {
	switch typed := value.(type) {
	case string:
		if typed != "" && typed != "null" {
			target["type"] = typed
		}
	case []string:
		normalizeSchemaTypeList(target, typed)
	case []any:
		types := make([]string, 0, len(typed))
		for _, item := range typed {
			if text, ok := item.(string); ok {
				types = append(types, text)
			}
		}
		normalizeSchemaTypeList(target, types)
	}
}

func normalizeSchemaTypeList(target map[string]any, types []string) {
	nullable := false
	for _, schemaType := range types {
		if schemaType == "null" {
			nullable = true
			continue
		}
		if schemaType != "" {
			target["type"] = schemaType
			break
		}
	}
	if nullable {
		target["nullable"] = true
	}
}

func normalizeSchemaRequired(value any) ([]string, bool) {
	switch typed := value.(type) {
	case []string:
		return append([]string(nil), typed...), true
	case []any:
		required := make([]string, 0, len(typed))
		for _, item := range typed {
			if text, ok := item.(string); ok && text != "" {
				required = append(required, text)
			}
		}
		return required, true
	default:
		return nil, false
	}
}

func sanitizeSchemaProperties(value any, depth int) map[string]any {
	properties, ok := value.(map[string]any)
	if !ok {
		return map[string]any{}
	}

	cleaned := make(map[string]any, len(properties))
	for name, property := range properties {
		if normalized, ok := normalizeSchemaObjectForProperty(property, depth); ok {
			cleaned[name] = normalized
		}
	}
	return cleaned
}

func normalizeSchemaObjectForProperty(value any, depth int) (map[string]any, bool) {
	switch typed := value.(type) {
	case map[string]any:
		return sanitizeToolSchemaMap(typed, depth), true
	case string:
		if typed == "" || typed == "null" {
			return nil, false
		}
		return map[string]any{"type": typed}, true
	case bool:
		if typed {
			return map[string]any{}, true
		}
		return nil, false
	case []any:
		if items, ok := normalizeSchemaObjectList(typed, depth); ok {
			return map[string]any{"anyOf": items}, true
		}
		return nil, false
	default:
		return nil, false
	}
}

func normalizeAdditionalProperties(value any, depth int) (any, bool) {
	switch typed := value.(type) {
	case bool:
		return typed, true
	case map[string]any:
		return sanitizeToolSchemaMap(typed, depth), true
	case string:
		if typed == "" || typed == "null" {
			return nil, false
		}
		return map[string]any{"type": typed}, true
	case []any:
		if items, ok := normalizeSchemaObjectList(typed, depth); ok {
			return map[string]any{"anyOf": items}, true
		}
	}
	return nil, false
}

func normalizeSchemaObject(value any, depth int) any {
	if normalized, ok := normalizeSchemaObjectForProperty(value, depth); ok {
		return normalized
	}
	return sanitizeToolSchemaValue(value, depth)
}

func normalizeSchemaObjectList(value any, depth int) ([]any, bool) {
	items, ok := value.([]any)
	if !ok {
		return nil, false
	}

	cleaned := make([]any, 0, len(items))
	for _, item := range items {
		if normalized, ok := normalizeSchemaObjectForProperty(item, depth); ok {
			cleaned = append(cleaned, normalized)
		}
	}
	return cleaned, len(cleaned) > 0
}
