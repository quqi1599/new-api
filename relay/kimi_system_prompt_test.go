package relay

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestKimiDynamicToolMessagesNeverReceiveSystemPrompt(t *testing.T) {
	for _, withPrompt := range []bool{false, true} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{ChannelSetting: dto.ChannelSettings{SystemPrompt: "channel prompt", SystemPromptOverride: true}}}
		request := &dto.GeneralOpenAIRequest{Model: "kimi-k3", Messages: []dto.Message{
			{Role: "system", Tools: json.RawMessage(`[{"type":"function","function":{"name":"lookup"}}]`)},
			{Role: "user", Content: "test"},
		}}
		if withPrompt {
			request.Messages = append(request.Messages, dto.Message{Role: "system", Content: "user prompt"})
		}
		applySystemPromptIfNeeded(c, info, request)
		wire, err := common.Marshal(request)
		require.NoError(t, err)
		var body struct {
			Messages []map[string]json.RawMessage `json:"messages"`
		}
		require.NoError(t, common.Unmarshal(wire, &body))
		tools, prompts := 0, 0
		for _, message := range body.Messages {
			if len(message["tools"]) > 0 {
				tools++
				require.NotContains(t, message, "content")
			}
			if string(message["role"]) == `"system"` && len(message["content"]) > 0 {
				prompts++
			}
		}
		require.Equal(t, 1, tools)
		require.Equal(t, 1, prompts)
	}
}
