package openaicompat

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/types"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func invalidOutputFormat(message string) error {
	return types.NewErrorWithStatusCode(errors.New(message), "invalid_output_format", http.StatusBadRequest, types.ErrOptionWithSkipRetry())
}

func outputFormatObject(raw json.RawMessage, label string) (map[string]json.RawMessage, error) {
	if common.GetJsonType(raw) != "object" {
		return nil, invalidOutputFormat(label + " must be an object")
	}
	var fields map[string]json.RawMessage
	if err := common.Unmarshal(raw, &fields); err != nil {
		return nil, invalidOutputFormat(label + " is invalid JSON")
	}
	return fields, nil
}

func formatString(raw json.RawMessage, required bool, label string) (string, error) {
	if len(raw) == 0 && !required {
		return "", nil
	}
	var value string
	if common.GetJsonType(raw) != "string" || common.Unmarshal(raw, &value) != nil || required && strings.TrimSpace(value) == "" {
		return "", invalidOutputFormat(label + " must be a non-empty string")
	}
	return value, nil
}

func checkFormatKeys(fields map[string]json.RawMessage, allowed ...string) error {
	for key := range fields {
		ok := false
		for _, name := range allowed {
			if key == name {
				ok = true
				break
			}
		}
		if !ok {
			return types.NewOutputFormatRouteError("output format extension cannot be preserved on this route")
		}
	}
	return nil
}

func validateSchemaDefinition(raw json.RawMessage, requireName bool) (map[string]json.RawMessage, error) {
	fields, err := outputFormatObject(raw, "JSON Schema definition")
	if err != nil {
		return nil, err
	}
	if err = checkFormatKeys(fields, "type", "name", "description", "strict", "schema"); err != nil {
		return nil, err
	}
	if _, err = formatString(fields["name"], requireName, "JSON Schema name"); err != nil {
		return nil, err
	}
	if _, err = formatString(fields["description"], false, "JSON Schema description"); err != nil {
		return nil, err
	}
	if common.GetJsonType(fields["schema"]) != "object" {
		return nil, invalidOutputFormat("json_schema requires a schema object")
	}
	if strict := fields["strict"]; len(strict) > 0 && string(strict) != "null" {
		var value bool
		if common.Unmarshal(strict, &value) != nil {
			return nil, invalidOutputFormat("JSON Schema strict must be a boolean")
		}
	}
	return fields, nil
}

// ApplyChatOutputFormatToClaude follows the paired CPA structured-output
// contract. Messages has no generic json_object or explicitly non-strict mode.
// Keep schemas as raw JSON so large integers and explicit zero/false survive.
func ApplyChatOutputFormatToClaude(format *dto.ResponseFormat, target *dto.ClaudeRequest) error {
	if format == nil {
		return nil
	}
	if len(format.ExtraFields) > 0 {
		return types.NewOutputFormatRouteError("Chat output format extension cannot be preserved by Messages")
	}
	switch format.Type {
	case "text":
		if len(format.JsonSchema) > 0 {
			return invalidOutputFormat("text format cannot include json_schema")
		}
		return nil
	case "json_object":
		if len(format.JsonSchema) > 0 {
			return invalidOutputFormat("json_object format cannot include json_schema")
		}
		return types.NewOutputFormatRouteError("JSON object mode requires a compatible OpenAI route; Messages cannot preserve json_object")
	case "json_schema":
	default:
		return types.NewOutputFormatRouteError("output format type cannot be preserved by Messages")
	}
	definition, err := validateSchemaDefinition(format.JsonSchema, true)
	if err != nil {
		return err
	}
	if _, exists := definition["type"]; exists {
		return types.NewOutputFormatRouteError("Chat JSON Schema definition extension cannot be preserved by Messages")
	}
	if gjson.ParseBytes(definition["strict"]).Type == gjson.False {
		return types.NewOutputFormatRouteError("Messages cannot preserve an explicitly non-strict JSON Schema")
	}
	schema := append([]byte(nil), definition["schema"]...)
	name, _ := formatString(definition["name"], true, "JSON Schema name")
	description, _ := formatString(definition["description"], false, "JSON Schema description")
	// Labels become annotations without altering the schema's accepted values.
	// Existing annotations and every original numeric literal remain intact.
	if name != "" && !gjson.GetBytes(schema, "title").Exists() {
		schema, err = sjson.SetBytes(schema, "title", name)
		if err != nil {
			return err
		}
	}
	if description != "" {
		if previous := gjson.GetBytes(schema, "description"); previous.Exists() {
			if previous.Type != gjson.String {
				return types.NewOutputFormatRouteError("schema description annotation cannot be preserved on this route")
			}
			if previous.String() != "" {
				description = previous.String() + "\n" + description
			}
		}
		schema, err = sjson.SetBytes(schema, "description", description)
		if err != nil {
			return err
		}
	}
	wire, err := sjson.SetRawBytes([]byte(`{"type":"json_schema"}`), "schema", schema)
	if err != nil {
		return err
	}
	config := target.OutputConfig
	if len(config) == 0 || string(config) == "null" {
		config = []byte(`{}`)
	}
	target.OutputConfig, err = sjson.SetRawBytes(config, "format", wire)
	return err
}

// ReadClaudeOutputFormat validates only format control fields. A native route
// cannot accept a conflicting legacy/new format or unsupported format extension.
func ReadClaudeOutputFormat(request *dto.ClaudeRequest) (json.RawMessage, error) {
	if request == nil {
		return nil, nil
	}
	var raw json.RawMessage
	if len(request.OutputConfig) > 0 && string(request.OutputConfig) != "null" {
		config, err := outputFormatObject(request.OutputConfig, "output_config")
		if err != nil {
			return nil, err
		}
		raw = config["format"]
	}
	if len(request.OutputFormat) > 0 && string(request.OutputFormat) != "null" {
		if len(raw) > 0 {
			return nil, invalidOutputFormat("output_format conflicts with output_config.format")
		}
		raw = request.OutputFormat
	}
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	fields, err := outputFormatObject(raw, "Messages output format")
	if err != nil {
		return nil, err
	}
	if err = checkFormatKeys(fields, "type", "schema"); err != nil {
		return nil, err
	}
	kind, err := formatString(fields["type"], true, "Messages output format type")
	if err != nil {
		return nil, err
	}
	if kind != "json_schema" {
		return nil, types.NewOutputFormatRouteError("Messages output format type cannot be preserved on this route")
	}
	if common.GetJsonType(fields["schema"]) != "object" {
		return nil, invalidOutputFormat("Messages json_schema requires a schema object")
	}
	return raw, nil
}

func ClaudeOutputFormatToChat(request *dto.ClaudeRequest) (*dto.ResponseFormat, error) {
	raw, err := ReadClaudeOutputFormat(request)
	if err != nil || len(raw) == 0 {
		return nil, err
	}
	definition, err := sjson.SetRawBytes([]byte(`{"name":"response","strict":true}`), "schema", []byte(gjson.GetBytes(raw, "schema").Raw))
	if err != nil {
		return nil, err
	}
	return &dto.ResponseFormat{Type: "json_schema", JsonSchema: definition}, nil
}

func SetClaudeOutputEffort(request *dto.ClaudeRequest, effort string) error {
	config := request.OutputConfig
	if len(config) == 0 || string(config) == "null" {
		config = []byte(`{}`)
	}
	if _, err := outputFormatObject(config, "output_config"); err != nil {
		return err
	}
	var err error
	request.OutputConfig, err = sjson.SetBytes(config, "effort", effort)
	return err
}

func chatFormatToResponsesText(format *dto.ResponseFormat, verbosity json.RawMessage) (json.RawMessage, error) {
	text := map[string]json.RawMessage{}
	if len(verbosity) > 0 {
		text["verbosity"] = verbosity
	}
	if format != nil {
		if len(format.ExtraFields) > 0 {
			return nil, types.NewOutputFormatRouteError("Chat output format extension cannot be preserved by Responses")
		}
		var wire []byte
		var err error
		switch format.Type {
		case "text", "json_object":
			if len(format.JsonSchema) > 0 {
				return nil, invalidOutputFormat("non-schema format cannot include json_schema")
			}
			wire, err = common.Marshal(map[string]string{"type": format.Type})
		case "json_schema":
			fields, validationErr := validateSchemaDefinition(format.JsonSchema, true)
			if validationErr != nil {
				return nil, validationErr
			}
			if _, exists := fields["type"]; exists {
				return nil, types.NewOutputFormatRouteError("Chat JSON Schema definition extension cannot be preserved by Responses")
			}
			wire, err = sjson.SetBytes(format.JsonSchema, "type", "json_schema")
		default:
			return nil, types.NewOutputFormatRouteError("Chat output format type cannot be preserved by Responses")
		}
		if err != nil {
			return nil, err
		}
		text["format"] = wire
	}
	if len(text) == 0 {
		return nil, nil
	}
	return common.Marshal(text)
}

func responsesTextToChatFormat(text json.RawMessage) (*dto.ResponseFormat, json.RawMessage, error) {
	if len(text) == 0 || string(text) == "null" {
		return nil, nil, nil
	}
	fields, err := outputFormatObject(text, "Responses text")
	if err != nil {
		return nil, nil, err
	}
	if err = checkFormatKeys(fields, "format", "verbosity"); err != nil {
		return nil, nil, err
	}
	raw := fields["format"]
	if len(raw) == 0 || string(raw) == "null" {
		return nil, fields["verbosity"], nil
	}
	formatFields, err := outputFormatObject(raw, "Responses text.format")
	if err != nil {
		return nil, nil, err
	}
	kind, err := formatString(formatFields["type"], true, "Responses format type")
	if err != nil {
		return nil, nil, err
	}
	switch kind {
	case "text", "json_object":
		if err = checkFormatKeys(formatFields, "type"); err != nil {
			return nil, nil, err
		}
		return &dto.ResponseFormat{Type: kind}, fields["verbosity"], nil
	case "json_schema":
		if _, err = validateSchemaDefinition(raw, true); err != nil {
			return nil, nil, err
		}
		definition, err := sjson.DeleteBytes(raw, "type")
		return &dto.ResponseFormat{Type: kind, JsonSchema: definition}, fields["verbosity"], err
	default:
		return nil, nil, types.NewOutputFormatRouteError("Responses output format type cannot be preserved on this route")
	}
}
