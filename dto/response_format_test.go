package dto

import (
	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"testing"
)

func TestResponseFormatDoesNotSilentlyDropExtensionsOrLargeNumbers(t *testing.T) {
	var request GeneralOpenAIRequest
	require.NoError(t, common.UnmarshalJsonStr(`{"response_format":{"type":"json_schema","json_schema":{"name":"result","strict":false,"schema":{"const":9007199254740993}},"future_zero":0,"future_false":false}}`, &request))
	require.Len(t, request.ResponseFormat.ExtraFields, 2)
	encoded, err := common.Marshal(request)
	require.NoError(t, err)
	require.Equal(t, "9007199254740993", gjson.GetBytes(encoded, "response_format.json_schema.schema.const").Raw)
	require.Equal(t, "false", gjson.GetBytes(encoded, "response_format.json_schema.strict").Raw)
	require.Equal(t, "0", gjson.GetBytes(encoded, "response_format.future_zero").Raw)
	require.Equal(t, "false", gjson.GetBytes(encoded, "response_format.future_false").Raw)
}
