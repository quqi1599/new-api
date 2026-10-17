package dto

import (
	"encoding/json"
	"github.com/QuantumNous/new-api/common"
)

// Preserve provider extensions on native Chat routes, so a cross-protocol
// converter can explicitly reject unsupported fields instead of losing them.
func (f *ResponseFormat) UnmarshalJSON(data []byte) error {
	type plain ResponseFormat
	var value plain
	if err := common.Unmarshal(data, &value); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := common.Unmarshal(data, &fields); err != nil {
		return err
	}
	delete(fields, "type")
	delete(fields, "json_schema")
	*f = ResponseFormat(value)
	if len(fields) > 0 {
		f.ExtraFields = fields
	}
	return nil
}

func (f ResponseFormat) MarshalJSON() ([]byte, error) {
	fields := make(map[string]json.RawMessage, len(f.ExtraFields)+2)
	for name, value := range f.ExtraFields {
		fields[name] = value
	}
	if f.Type != "" {
		encoded, err := common.Marshal(f.Type)
		if err != nil {
			return nil, err
		}
		fields["type"] = encoded
	}
	if len(f.JsonSchema) > 0 {
		fields["json_schema"] = f.JsonSchema
	}
	return common.Marshal(fields)
}
