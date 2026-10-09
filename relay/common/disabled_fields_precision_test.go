package common

import (
	"testing"

	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/tidwall/gjson"
)

func setDisabledFieldsPassThroughForTest(t *testing.T, enabled bool) {
	t.Helper()
	settings := model_setting.GetGlobalSettings()
	previous := settings.PassThroughRequestEnabled
	settings.PassThroughRequestEnabled = enabled
	t.Cleanup(func() { settings.PassThroughRequestEnabled = previous })
}

func TestRemoveDisabledFieldsPreservesExactJSONValues(t *testing.T) {
	setDisabledFieldsPassThroughForTest(t, false)
	input := []byte(`{
		"service_tier":"priority","inference_geo":"us","speed":"fast",
		"safety_identifier":"synthetic-user","store":false,
		"integer":9007199254740993,"negative":-9007199254740993,
		"decimal":0.123456789012345678901234567890,
		"exponent":1.230000000000000000000000001e+40,"negative_zero":-0,
		"response_format":{"type":"json_schema","json_schema":{"name":"synthetic_precision","strict":false,"schema":{"type":"object","properties":{"n":{"enum":[9007199254740993,-9007199254740993,0.123456789012345678901234567890]}}}}},
		"metadata":{"nested":[{"counter":9007199254740993,"values":[null,false,"9007199254740993"]}]},
		"stream_options":{"include_obfuscation":false,"include_usage":false,"counter":9007199254740993,"decimal":0.123456789012345678901234567890,"nested":{"enum":[9007199254740993]}}
	}`)
	out, err := RemoveDisabledFields(input, dto.ChannelOtherSettings{}, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"service_tier", "inference_geo", "speed", "safety_identifier", "stream_options.include_obfuscation"} {
		if gjson.GetBytes(out, field).Exists() {
			t.Errorf("disabled field %s remained", field)
		}
	}
	for _, path := range []string{
		"integer", "negative", "decimal", "exponent", "negative_zero", "store",
		"response_format.json_schema.strict",
		"response_format.json_schema.schema.properties.n.enum.0",
		"response_format.json_schema.schema.properties.n.enum.1",
		"response_format.json_schema.schema.properties.n.enum.2",
		"metadata.nested.0.counter", "metadata.nested.0.values.0",
		"metadata.nested.0.values.1", "metadata.nested.0.values.2",
		"stream_options.include_usage", "stream_options.counter",
		"stream_options.decimal", "stream_options.nested.enum.0",
	} {
		want, got := gjson.GetBytes(input, path).Raw, gjson.GetBytes(out, path).Raw
		if got != want {
			t.Errorf("%s changed: want exact JSON %s, got %s", path, want, got)
		}
	}
}

func TestRemoveDisabledFieldsPreservesFilteringControls(t *testing.T) {
	setDisabledFieldsPassThroughForTest(t, false)
	input := []byte(`{"service_tier":"priority","inference_geo":"us","speed":"fast","safety_identifier":"synthetic-user","store":false,"stream_options":{"include_obfuscation":false,"include_usage":true},"counter":9007199254740993}`)
	settings := dto.ChannelOtherSettings{
		AllowServiceTier: true, AllowInferenceGeo: true, AllowSpeed: true,
		AllowSafetyIdentifier: true, AllowIncludeObfuscation: true, DisableStore: true,
	}
	out, err := RemoveDisabledFields(input, settings, false)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(out, "store").Exists() {
		t.Fatal("explicitly disabled store remained")
	}
	for _, path := range []string{"service_tier", "inference_geo", "speed", "safety_identifier", "stream_options.include_obfuscation", "stream_options.include_usage", "counter"} {
		if got, want := gjson.GetBytes(out, path).Raw, gjson.GetBytes(input, path).Raw; got != want {
			t.Errorf("allowed field %s changed: want %s, got %s", path, want, got)
		}
	}
}

func TestRemoveDisabledFieldsPreservesStreamOptionsShapes(t *testing.T) {
	setDisabledFieldsPassThroughForTest(t, false)
	for _, tc := range []struct {
		name, raw, want string
		allow           bool
	}{
		{"null", `null`, `null`, false},
		{"empty array", `[]`, `[]`, false},
		{"array with exact integer", `[9007199254740993]`, `[9007199254740993]`, false},
		{"boolean", `false`, `false`, false},
		{"string", `"synthetic"`, `"synthetic"`, false},
		{"number", `9007199254740993`, `9007199254740993`, false},
		{"empty object", `{}`, ``, false},
		{"only disabled field", `{"include_obfuscation":false}`, ``, false},
		{"disabled null field", `{"include_obfuscation":null}`, ``, false},
		{"remaining usage", `{"include_obfuscation":true,"include_usage":false}`, `{"include_usage":false}`, false},
		{"remaining nested null", `{"include_obfuscation":false,"future":null}`, `{"future":null}`, false},
		{"allowed empty object", `{}`, `{}`, true},
		{"allowed obfuscation", `{"include_obfuscation":false}`, `{"include_obfuscation":false}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := []byte(`{"stream_options":` + tc.raw + `}`)
			out, err := RemoveDisabledFields(input, dto.ChannelOtherSettings{AllowIncludeObfuscation: tc.allow}, false)
			if err != nil {
				t.Fatal(err)
			}
			if got := gjson.GetBytes(out, "stream_options").Raw; got != tc.want {
				t.Fatalf("stream_options shape or value changed: want %q, got %q", tc.want, got)
			}
		})
	}
}

func TestRemoveDisabledFieldsPreservesInvalidAndNonObjectFallback(t *testing.T) {
	setDisabledFieldsPassThroughForTest(t, false)
	for _, input := range []string{
		``, `{"unfinished":`, `{"n":01}`, `{"n":1} {"n":2}`,
		` [9007199254740993] `, ` "synthetic" `, ` false `, ` 9007199254740993 `,
	} {
		out, err := RemoveDisabledFields([]byte(input), dto.ChannelOtherSettings{}, false)
		if err != nil || string(out) != input {
			t.Errorf("existing fallback changed for synthetic input %q: out=%q err=%v", input, out, err)
		}
	}
	out, err := RemoveDisabledFields([]byte(" \n null \t"), dto.ChannelOtherSettings{}, false)
	if err != nil || string(out) != "null" {
		t.Fatalf("top-level null changed: out=%q err=%v", out, err)
	}
}

func TestRemoveDisabledFieldsPassThroughPreservesExactBytes(t *testing.T) {
	for _, global := range []bool{false, true} {
		t.Run(map[bool]string{false: "channel", true: "global"}[global], func(t *testing.T) {
			setDisabledFieldsPassThroughForTest(t, global)
			for _, input := range []string{
				" {\n\"service_tier\":\"priority\", \"n\":9007199254740993, \"stream_options\":{\"include_obfuscation\":false}} \n",
				`{"unfinished":`, ` null `, ` [9007199254740993] `,
			} {
				out, err := RemoveDisabledFields([]byte(input), dto.ChannelOtherSettings{}, !global)
				if err != nil || string(out) != input {
					t.Errorf("pass-through changed bytes: out=%q err=%v", out, err)
				}
			}
		})
	}
}
