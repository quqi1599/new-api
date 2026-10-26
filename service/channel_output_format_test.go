package service

import (
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestJSONModeChannelPreflight(t *testing.T) {
	for _, tc := range []struct {
		path, chat, text string
		blocked          bool
	}{
		{"/v1/chat/completions", `{"type":"json_object"}`, "", true},
		{"/pg/chat/completions", `{"type":"json_object"}`, "", true},
		{"/v1/responses", "", `{"format":{"type":"json_object"}}`, true},
		{"/v1/chat/completions", `{"type":"json_schema"}`, "", false},
		{"/v1/chat/completions", `{"type":"text"}`, "", false},
		{"/v1/chat/completions", `null`, "", false},
		{"/v1/messages", `{"type":"json_object"}`, "", false},
	} {
		t.Run(tc.path+tc.chat+tc.text, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("POST", tc.path, nil)
			SetRequestJSONMode(c, []byte(tc.chat), []byte(tc.text))
			channel := &model.Channel{Type: constant.ChannelTypeAnthropic}
			err := PreflightChannelOutputFormat(c, channel)
			require.Equal(t, tc.blocked, types.IsLocalOutputFormatRouteError(err))
			if tc.blocked {
				require.True(t, IsLocalChannelCandidateRejection(err))
				require.False(t, err.HasUpstreamResponse())
			}
			for _, kind := range []int{constant.ChannelTypeOpenAI, constant.ChannelTypeGemini, constant.ChannelTypeAzure} {
				require.Nil(t, PreflightChannelOutputFormat(c, &model.Channel{Type: kind}))
			}
			channel.Setting = common.GetPointer(`{"pass_through_body_enabled":true}`)
			require.Nil(t, PreflightChannelOutputFormat(c, channel))
		})
	}
}

func TestJSONModePreflightPreservesGlobalPassthrough(t *testing.T) {
	setting := model_setting.GetGlobalSettings()
	old := setting.PassThroughRequestEnabled
	t.Cleanup(func() { setting.PassThroughRequestEnabled = old })
	setting.PassThroughRequestEnabled = true
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
	SetRequestJSONMode(c, []byte(`{"type":"json_object"}`), nil)
	require.Nil(t, PreflightChannelOutputFormat(c, &model.Channel{Type: constant.ChannelTypeAnthropic}))
}
