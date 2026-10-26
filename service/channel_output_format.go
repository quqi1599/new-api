package service

import (
	"encoding/json"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const requestJSONModeKey = "relay_request_json_object"

// Remember the client's format once during intake. Retry channel setup must
// inspect the original requirement, not a previous attempt's converted body.
func SetRequestJSONMode(c *gin.Context, chatFormat, responsesText json.RawMessage) {
	if c == nil || c.Request == nil {
		return
	}
	var kind gjson.Result
	switch c.Request.URL.Path {
	case "/v1/chat/completions", "/pg/chat/completions":
		kind = gjson.GetBytes(chatFormat, "type")
	case "/v1/responses":
		kind = gjson.GetBytes(responsesText, "format.type")
	}
	c.Set(requestJSONModeKey, kind.Type == gjson.String && kind.Str == "json_object")
}

// PreflightChannelOutputFormat skips a known impossible conversion before key
// selection, billing or an upstream attempt. Other providers retain their
// existing adapters and format validation. Explicit passthrough is unchanged.
func PreflightChannelOutputFormat(c *gin.Context, channel *model.Channel) *types.NewAPIError {
	if c == nil || channel == nil || !c.GetBool(requestJSONModeKey) || channel.Type != constant.ChannelTypeAnthropic {
		return nil
	}
	if model_setting.GetGlobalSettings().PassThroughRequestEnabled || channel.GetSetting().PassThroughBodyEnabled {
		return nil
	}
	return types.NewOutputFormatRouteError("JSON object mode requires a compatible OpenAI route; Messages cannot preserve json_object")
}

// Only local, pre-send candidate failures may consume the candidate-skip
// budget instead of the upstream attempt budget.
func IsLocalChannelCandidateRejection(err *types.NewAPIError) bool {
	return err != nil && (err.GetErrorCode() == types.ErrorCodeChannelNoAvailableKey || types.IsLocalOutputFormatRouteError(err))
}
